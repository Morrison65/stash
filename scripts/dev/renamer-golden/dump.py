# Read-only dump of organized scenes (renamerOnUpdate's SceneData shape) and studios.
import json, sys, urllib.request

URL = "http://localhost:9999/graphql"

def gql(query, variables=None):
    req = urllib.request.Request(URL, json.dumps({"query": query, "variables": variables or {}}).encode(),
                                 {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        out = json.load(r)
    if out.get("errors"):
        raise SystemExit(out["errors"])
    return out["data"]

SCENE = """
    id title date rating100 organized code
    stash_ids { endpoint stash_id }
    files { path video_codec audio_codec width height frame_rate duration bit_rate
            phash: fingerprint(type: "phash") oshash: fingerprint(type: "oshash") checksum: fingerprint(type: "checksum")
            fingerprints { type value } }
    studio { id name parent_studio { id name } }
    tags { id name sort_name aliases }
    performers { id name gender favorite rating100 stash_ids { endpoint stash_id } }
    movies { movie { name date } scene_index }
"""
q = "query($f: SceneFilterType) { findScenes(scene_filter: $f, filter: {per_page: -1, sort: \"id\", direction: ASC}) { count scenes {" + SCENE + "} } }"
scenes = gql(q, {"f": {"organized": True}})["findScenes"]
studios = gql("{ findStudios(filter: {per_page: -1}) { studios { id name parent_studio { id name } } } }")["findStudios"]["studios"]
json.dump({"scenes": scenes["scenes"], "studios": studios}, open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False)
print("organized scenes:", scenes["count"], "studios:", len(studios))
