package manager

import (
	"github.com/stashapp/stash/pkg/ffmpeg"
	"github.com/stashapp/stash/pkg/logger"
)

func logErrorOutput(err error) {
	// covers both local (*exec.ExitError) and stash-fsd processes
	if stderr := ffmpeg.StderrOf(err); len(stderr) > 0 {
		logger.Errorf("command stderr: %v", string(stderr))
	}
}
