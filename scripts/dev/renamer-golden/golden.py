# Golden output of the original renamerOnUpdate plugin (hook mode) for every
# scene in data.json. Runs the plugin's own code with all side effects stubbed:
# file_rename records the computed target instead of moving anything.
#
# usage: python golden.py <plugin dir> <data.json> <maps file> <out>
#
# Paths: the plugin ran in the classic container on /data/... paths, so
# canonical library paths are mapped back to /data for the plugin and its
# results are mapped forward again.
import copy
import io
import json
import sys
import types

plugin_dir, data_path, maps_path, out_path = sys.argv[1:5]

maps = []
for line in open(maps_path, encoding="utf-8"):
    line = line.strip()
    if line:
        data_prefix, canon = line.split("=", 1)
        maps.append((data_prefix, canon))


def remap(path, frm, to):
    best = None
    for m in maps:
        p = m[frm]
        if path == p or path.startswith(p + "/"):
            if best is None or len(p) > len(best[frm]):
                best = m
    if best is None:
        return None
    return best[to] + path[len(best[frm]):]


data = json.load(open(data_path, encoding="utf-8"))
studios = {s["id"]: s for s in data["studios"]}

# --- stubs -----------------------------------------------------------------
records = []  # messages of the current scene


def _log(level):
    return lambda s: records.append((level, str(s)))


log_mod = types.ModuleType("log")
for name, level in [("LogTrace", "trace"), ("LogDebug", "debug"), ("LogInfo", "info"),
                    ("LogWarning", "warning"), ("LogError", "error"), ("LogProgress", "progress")]:
    setattr(log_mod, name, _log(level))
log_mod.logWarning = _log("warning")  # typo used by the plugin
sys.modules["log"] = log_mod


class _Resp:
    status_code = 200

    def __init__(self, data):
        self._data = data

    def json(self):
        return {"data": self._data}


def _post(url, json=None, **kw):
    q = json["query"]
    v = json.get("variables") or {}
    if "databasePath" in q:
        return _Resp({"configuration": {"general": {"databasePath": "/nonexistent.sqlite"}}})
    if "databaseSchema" in q:
        return _Resp({"systemStatus": {"databaseSchema": 85}})
    if "findStudio" in q:
        return _Resp({"findStudio": studios.get(str(v["id"]))})
    raise RuntimeError("unexpected query: " + q[:80])


requests_mod = types.ModuleType("requests")
requests_mod.post = _post
sys.modules["requests"] = requests_mod

sys.path.insert(0, plugin_dir)
fragment = {
    "server_connection": {"PluginDir": plugin_dir, "Port": 9999, "Scheme": "http", "Host": "localhost",
                          "SessionCookie": {"Value": ""}},
    "args": {"hookContext": {"type": "Scene.Update.Post", "id": 0}},
}
sys.stdin = io.StringIO(json.dumps(fragment))

src = open(plugin_dir + "/renamerOnUpdate.py", encoding="utf-8").read()
cut = src.rindex("\nif PLUGIN_ARGS == MOVE_WORKER_MODE:")
ns = {"__name__": "renamerOnUpdate"}
exec(compile(src[:cut], "renamerOnUpdate.py", "exec"), ns)

orig_fmr = copy.deepcopy(ns["FIELD_MULTI_REPLACER"])

# deterministic tag order (the plugin uses set order)
_filter_tags = ns["filter_tags"]
ns["filter_tags"] = lambda tags: sorted(_filter_tags(tags))

status = {}  # file index -> (status, final, message)
state = {"paths": [], "idx": -1}

_extract_info = ns["extract_info"]


def extract_info(scene, template):
    state["idx"] = state["paths"].index(scene["path"])
    return _extract_info(scene, template)


_check_longpath = ns["check_longpath"]


def check_longpath(path):
    res = _check_longpath(path)
    if res:
        status[state["idx"]] = ("error", "", "path too long (%d > 240)" % len(path))
    return res


def file_rename(current_path, new_path, scene_info):
    status[state["idx"]] = ("move", new_path, "")
    return 1  # stop before any database or file operation


def _info(s):
    records.append(("info", str(s)))
    if str(s).startswith("Everything is ok"):
        status[state["idx"]] = ("same", None, "")


log_mod.LogInfo = _info
ns["extract_info"] = extract_info
ns["check_longpath"] = check_longpath
ns["file_rename"] = file_rename
ns["checking_duplicate_db"] = lambda scene_info: None
ns["other_filesystem"] = lambda path, directory: False


class _DB:
    def close(self):
        pass


out = open(out_path, "w", encoding="utf-8", newline="\n")
counts = {}


def emit(sid, idx, st, current, final, msg=""):
    counts[st] = counts.get(st, 0) + 1
    out.write("|".join([sid, str(idx), st, current or "", final or "", msg.replace("\n", " ")]) + "\n")


for scene in data["scenes"]:
    sid = scene["id"]
    canon_paths = [f["path"] for f in scene.get("files") or []]
    s = copy.deepcopy(scene)
    data_paths = [remap(p, 1, 0) for p in canon_paths]
    if None in data_paths:
        emit(sid, -1, "unmapped", canon_paths[0], "")
        continue
    for f, p in zip(s["files"], data_paths):
        f["path"] = p

    records.clear()
    status.clear()
    state["paths"], state["idx"] = data_paths, -1
    ns["FIELD_MULTI_REPLACER"] = copy.deepcopy(orig_fmr)
    try:
        ns["renamer"](s, _DB())
        exc = ""
    except Exception as e:  # the plugin logs "main function error" and exits
        exc = f"{type(e).__name__}: {e}"

    if any("Scene ignored (not organized)" in m for _, m in records):
        emit(sid, -1, "unorganized", canon_paths[0] if canon_paths else "", "")
        continue
    scene_errors = [m for lvl, m in records if lvl == "error"
                    and not m.startswith("Error during database operation")
                    and not m.startswith("The path is too long")]
    for i, cp in enumerate(canon_paths):
        st, final, msg = status.get(i, ("error", "", exc or (scene_errors[0] if scene_errors else "not processed")))
        if st == "move":
            final = remap(final, 0, 1) or ("UNMAPPED:" + final)
        elif st == "same":
            final = cp
        emit(sid, i, st, cp, final, msg)

out.close()
print(json.dumps(counts, sort_keys=True))
