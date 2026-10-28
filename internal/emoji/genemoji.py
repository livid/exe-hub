#!/usr/bin/env python3
"""Write the emoji a post.react may give, and their names, beside this
script (PLAN.md, Reactions — Which emoji):

    emoji.txt          every fully-qualified sequence in Unicode's
                       emoji-test.txt, one a line, in its order
    names/<lang>.json  [[emoji, name, keywords], ...] in the same order,
                       for en, zh (Simplified), zh-hant and ja, from
                       CLDR's annotations and derived annotations

Run it again when Unicode's emoji move on, with the version the preview
pictures were cut from (internal/preview/genemoji.py) and the CLDR
release that names that version's emoji:

    python3 -I internal/emoji/genemoji.py [--src DIR]

A name CLDR does not give in a language falls back to English, and one
it gives in no language to emoji-test.txt's own. English names are set
in title case, small words lowercase, as the mockups wrote them
("Face with Tears of Joy").
"""

import argparse
import json
import os
import re
import tempfile
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
UNICODE = "16.0"
CLDR = "46.0.0"
LANGS = {"en": "en", "zh": "zh", "zh-hant": "zh-Hant", "ja": "ja"}
BASE = f"https://raw.githubusercontent.com/unicode-org/cldr-json/{CLDR}/cldr-json"


def sources():
    out = {"emoji-test.txt": f"https://www.unicode.org/Public/emoji/{UNICODE}/emoji-test.txt"}
    for code in LANGS.values():
        out[f"ann-{code}.json"] = f"{BASE}/cldr-annotations-full/annotations/{code}/annotations.json"
        out[f"der-{code}.json"] = f"{BASE}/cldr-annotations-derived-full/annotationsDerived/{code}/annotations.json"
    return out


def fetch(src):
    os.makedirs(src, exist_ok=True)
    for name, url in sources().items():
        path = os.path.join(src, name)
        if not os.path.exists(path):
            urllib.request.urlretrieve(url, path)


def fully_qualified(path):
    """Each fully-qualified sequence and emoji-test.txt's name for it."""
    out = []
    for line in open(path, encoding="utf-8"):
        if line.startswith("#") or ";" not in line:
            continue
        seq, rest = line.split(";", 1)
        status, _, comment = rest.partition("#")
        if status.strip() != "fully-qualified":
            continue
        e = "".join(chr(int(cp, 16)) for cp in seq.split())
        # "# 😀 E1.0 grinning face": the name follows the version
        name = re.sub(r"^\S+\s+E\d+\.\d+\s+", "", comment.strip())
        out.append((e, name))
    return out


SMALL = {"a", "an", "and", "as", "at", "but", "by", "for", "from", "in", "into", "of", "on", "or", "the", "to", "with", "without"}


def title(name):
    """Title case as the mockups set a name: each word capitalised but
    the small ones, the first word after a colon too; a word with a
    capital in it already (Japan, ATM) is left as it is."""
    out, first = [], True
    for word in name.split(" "):
        parts = []
        for part in word.split("-"):
            bare = part.strip(":,()")
            if part and (first or bare.lower() not in SMALL) and part == part.lower():
                part = part[0].upper() + part[1:]
            parts.append(part)
        out.append("-".join(parts))
        first = word.endswith(":")
    return " ".join(out)


def names(src, code):
    table = {}
    for kind in ("ann", "der"):
        data = json.load(open(os.path.join(src, f"{kind}-{code}.json"), encoding="utf-8"))
        root = data["annotations"] if kind == "ann" else data["annotationsDerived"]
        for key, v in root["annotations"].items():
            tts = (v.get("tts") or [""])[0]
            if tts:
                table[key] = (tts, " ".join(v.get("default") or []))
    return table


def lookup(table, e):
    # CLDR keys an emoji without its U+FE0F
    return table.get(e) or table.get(e.replace("️", ""))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", default=os.path.join(tempfile.gettempdir(), "genemoji-src"), help="where the sources are, fetched when missing")
    args = ap.parse_args()
    fetch(args.src)
    seqs = fully_qualified(os.path.join(args.src, "emoji-test.txt"))
    with open(os.path.join(HERE, "emoji.txt"), "w", encoding="utf-8") as f:
        f.write("".join(e + "\n" for e, _ in seqs))
    en = names(args.src, "en")
    os.makedirs(os.path.join(HERE, "names"), exist_ok=True)
    for lang, code in LANGS.items():
        table, rows, missing = names(args.src, code), [], 0
        for e, own in seqs:
            got = lookup(table, e)
            if not got:
                missing += 1
                got = lookup(en, e) or (own, "")
            name, keys = got
            if lang == "en":
                name = title(name)
            # the keywords say more than the name: the ones it does not hold already
            keys = " ".join(k for k in keys.split(" ") if k and k.lower() not in name.lower())
            rows.append([e, name, keys] if keys else [e, name])
        with open(os.path.join(HERE, "names", lang + ".json"), "w", encoding="utf-8") as f:
            json.dump(rows, f, ensure_ascii=False, separators=(",", ":"))
            f.write("\n")
        print(f"{lang}: {len(rows)} emoji, {missing} named in English or by Unicode")


if __name__ == "__main__":
    main()
