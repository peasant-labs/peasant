import json, sys
r = json.load(open(sys.argv[1]))
for root in r["roots"]:
    print(f"root {root['path']}: present={root['present']} sessions={root['sessions']}")
print(f"captures: {len(r['captures'])}  failures: {len(r.get('failures') or [])}")
for c in r["captures"]:
    m = c["metadata"]
    extra = ""
    if m.get("title"):
        extra += f" title={m['title']!r}"
    if m.get("models"):
        extra += f" models={m['models']}"
    if m.get("userTurns") or m.get("assistantMessages") or m.get("toolCalls"):
        extra += f" user={m['userTurns']} assistant={m['assistantMessages']} toolCalls={m['toolCalls']}"
    print(f"- session {c['session']}{extra}")
    for s in c["artifacts"]:
        print(f"    {s['artifact']}: format={s['format']} role={s['role']} records={s['records']} kinds={s.get('kinds')}")
