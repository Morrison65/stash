//! A counting semaphore whose waiters are served by priority class
//! (interactive before normal before background, FIFO within a class).

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use tokio::sync::oneshot;

#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Class {
    Interactive = 0,
    Normal = 1,
    Background = 2,
}

impl Class {
    pub fn from_proto(p: i32) -> Class {
        match crate::pb::Priority::try_from(p).unwrap_or(crate::pb::Priority::Unspecified) {
            crate::pb::Priority::Interactive => Class::Interactive,
            crate::pb::Priority::Background => Class::Background,
            _ => Class::Normal,
        }
    }
}

struct State {
    available: usize,
    waiters: [VecDeque<oneshot::Sender<Permit>>; 3],
}

pub struct PrioritySemaphore {
    state: Mutex<State>,
}

/// Releases its slot on drop (handing it to the best waiter, if any).
pub struct Permit {
    sem: Arc<PrioritySemaphore>,
}

impl Drop for Permit {
    fn drop(&mut self) {
        self.sem.release();
    }
}

impl PrioritySemaphore {
    pub fn new(capacity: usize) -> Arc<PrioritySemaphore> {
        Arc::new(PrioritySemaphore {
            state: Mutex::new(State {
                available: capacity,
                waiters: [VecDeque::new(), VecDeque::new(), VecDeque::new()],
            }),
        })
    }

    #[cfg(test)]
    pub fn available(&self) -> usize {
        self.state.lock().unwrap().available
    }

    pub async fn acquire(self: &Arc<Self>, class: Class) -> Permit {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if st.available > 0 {
                st.available -= 1;
                return Permit { sem: self.clone() };
            }
            let (tx, rx) = oneshot::channel();
            st.waiters[class as usize].push_back(tx);
            rx
        };
        // The sender is only dropped by `release` after a successful send, so
        // an error here cannot happen; fall back to retrying defensively.
        match rx.await {
            Ok(p) => p,
            Err(_) => Box::pin(self.acquire(class)).await,
        }
    }

    fn release(self: &Arc<Self>) {
        let mut st = self.state.lock().unwrap();
        for q in 0..3 {
            while let Some(tx) = st.waiters[q].pop_front() {
                match tx.send(Permit { sem: self.clone() }) {
                    Ok(()) => return,
                    // Waiter was cancelled: reclaim the permit without running
                    // its Drop (we hold the lock) and try the next one.
                    Err(p) => std::mem::forget(p),
                }
            }
        }
        st.available += 1;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[tokio::test]
    async fn interactive_goes_first() {
        let sem = PrioritySemaphore::new(1);
        let first = sem.acquire(Class::Normal).await;
        let order = Arc::new(Mutex::new(Vec::new()));
        let mut handles = Vec::new();
        for (i, class) in [Class::Background, Class::Normal, Class::Interactive, Class::Normal].into_iter().enumerate() {
            let sem = sem.clone();
            let order = order.clone();
            handles.push(tokio::spawn(async move {
                let _p = sem.acquire(class).await;
                order.lock().unwrap().push((i, class));
                tokio::time::sleep(Duration::from_millis(5)).await;
            }));
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        drop(first);
        for h in handles {
            h.await.unwrap();
        }
        let got: Vec<Class> = order.lock().unwrap().iter().map(|x| x.1).collect();
        assert_eq!(got, vec![Class::Interactive, Class::Normal, Class::Normal, Class::Background]);
        let idx: Vec<usize> = order.lock().unwrap().iter().map(|x| x.0).collect();
        assert_eq!(idx, vec![2, 1, 3, 0]); // FIFO within a class
        assert_eq!(sem.available(), 1);
    }

    #[tokio::test]
    async fn cancelled_waiters_do_not_leak() {
        let sem = PrioritySemaphore::new(2);
        let a = sem.acquire(Class::Normal).await;
        let b = sem.acquire(Class::Normal).await;
        let s2 = sem.clone();
        let waiter = tokio::spawn(async move {
            let _p = s2.acquire(Class::Interactive).await;
        });
        tokio::time::sleep(Duration::from_millis(10)).await;
        waiter.abort();
        let _ = waiter.await;
        drop(a);
        drop(b);
        assert_eq!(sem.available(), 2);
        let _x = sem.acquire(Class::Background).await;
        let _y = sem.acquire(Class::Background).await;
        assert_eq!(sem.available(), 0);
    }

    #[test]
    fn proto_mapping() {
        assert_eq!(Class::from_proto(1), Class::Interactive);
        assert_eq!(Class::from_proto(2), Class::Normal);
        assert_eq!(Class::from_proto(3), Class::Background);
        assert_eq!(Class::from_proto(0), Class::Normal);
        assert_eq!(Class::from_proto(99), Class::Normal);
    }
}
