#!/usr/bin/env python3
"""Generate compile-only Java stubs for android.telephony.ims.* from a device's
framework dex (dexdump text output).

The ImsService / MmTelFeature / ImsCallSessionImplBase APIs are @SystemApi and
missing (or only partly present) in the public android.jar. The IMS service
module compiles against these stubs placed *before* android.jar on the boot
classpath; the stubs are never packaged, the device framework provides the
real classes at run time.

Usage: gen_ims_stubs.py OUT_DIR ANDROID_JAR DEXDUMP_TXT...
"""
import re
import subprocess
import sys
from collections import defaultdict

PREFIXES = ("android/telephony/ims/",)

PRIM = {"V": "void", "Z": "boolean", "B": "byte", "C": "char", "S": "short",
        "I": "int", "J": "long", "F": "float", "D": "double"}


def parse(paths):
    """Returns {descriptor: class-dict} for classes under PREFIXES."""
    classes = {}
    for path in paths:
        cur = None
        section = None
        member = None
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                m = re.match(r"\s*Class descriptor\s*: '(L[^']+;)'", line)
                if m:
                    desc = m.group(1)
                    cur = None
                    if desc[1:].startswith(PREFIXES):
                        cur = {"desc": desc, "access": 0, "super": None, "ifaces": [],
                               "fields": [], "methods": []}
                        classes[desc] = cur
                    section = member = None
                    continue
                if cur is None:
                    continue
                s = line.strip()
                if s.startswith("Access flags") and section is None:
                    cur["access"] = int(re.search(r"0x([0-9a-f]+)", s).group(1), 16)
                elif s.startswith("Superclass"):
                    m = re.search(r"'(L[^']+;)'", s)
                    cur["super"] = m.group(1) if m else None
                elif s.startswith("Interfaces"):
                    section = "ifaces"
                elif s.startswith(("Static fields", "Instance fields")):
                    section = "fields"
                elif s.startswith(("Direct methods", "Virtual methods")):
                    section = "methods"
                elif s.startswith("source_file_idx"):
                    cur = None
                elif section == "ifaces":
                    m = re.match(r"#\d+\s*: '(L[^']+;)'", s)
                    if m:
                        cur["ifaces"].append(m.group(1))
                elif section in ("fields", "methods"):
                    if re.match(r"#\d+\s*:", s):
                        member = {}
                        cur[section].append(member)
                    elif member is not None:
                        m = re.match(r"(name|type|access|value)\s*: (.*)", s)
                        if m:
                            k, v = m.group(1), m.group(2)
                            if k == "access":
                                member[k] = int(re.search(r"0x([0-9a-f]+)", v).group(1), 16)
                            elif k in ("name", "type"):
                                member[k] = v.strip("'")
                            else:
                                member[k] = v
    return classes


def android_jar_classes(jar):
    out = subprocess.run(["unzip", "-Z1", jar], capture_output=True, text=True).stdout
    return {"L" + n[:-6] + ";" for n in out.split() if n.endswith(".class")}


def split_types(sig):
    """'(ILjava/lang/String;[B)V' -> (['I','Ljava/lang/String;','[B'], 'V')."""
    m = re.match(r"\((.*)\)(.*)", sig)
    args, ret, i = [], m.group(2), 0
    s = m.group(1)
    while i < len(s):
        j = i
        while s[j] == "[":
            j += 1
        if s[j] == "L":
            j = s.index(";", j)
        args.append(s[i:j + 1])
        i = j + 1
    return args, ret


def base(t):
    return t.lstrip("[")


def java(t):
    dims = len(t) - len(t.lstrip("["))
    b = base(t)
    name = PRIM[b] if b in PRIM else b[1:-1].replace("/", ".").replace("$", ".")
    return name + "[]" * dims


def main():
    out_dir, jar, dumps = sys.argv[1], sys.argv[2], sys.argv[3:]
    classes = parse(dumps)
    known = android_jar_classes(jar) | set(classes)

    def ok(t):
        b = base(t)
        return b in PRIM or b in known

    # Skip synthetic / lambda / annotation classes.
    def wanted(desc, c):
        name = desc[1:-1]
        if "$$" in name or re.search(r"\$\d+;?$", desc[:-1] + ";"[0:0]) or re.search(r"\$\d+$", name):
            return False
        if c["access"] & 0x1000 or c["access"] & 0x2000:  # SYNTHETIC / ANNOTATION
            return False
        if c["super"] == "Ljava/lang/Enum;":
            return False
        return True

    kept = {d: c for d, c in classes.items() if wanted(d, c)}
    known = android_jar_classes(jar) | set(kept)
    children = defaultdict(list)
    tops = []
    for d in sorted(kept):
        name = d[1:-1]
        if "$" in name:
            outer = "L" + name.rsplit("$", 1)[0] + ";"
            if outer in kept:
                children[outer].append(d)
                continue
        tops.append(d)

    def emit(desc, depth, nested):
        c = kept[desc]
        ind = "    " * depth
        simple = desc[1:-1].split("/")[-1].split("$")[-1]
        acc = c["access"]
        is_iface = bool(acc & 0x200)
        mods = ["public"]
        if nested:
            mods.append("static")
        if is_iface:
            kind = "interface"
        else:
            kind = "class"
            if acc & 0x400:
                mods.append("abstract")
        ext = ""
        if not is_iface and c["super"] and c["super"] != "Ljava/lang/Object;" and ok(c["super"]):
            ext = " extends " + java(c["super"])
        ifs = [java(i) for i in c["ifaces"] if ok(i)]
        if ifs:
            ext += (" extends " if is_iface else " implements ") + ", ".join(ifs)
        lines = [f"{ind}{' '.join(mods)} {kind} {simple}{ext} {{"]
        inner = ind + "    "
        for f in c["fields"]:
            a = f.get("access", 0)
            if not a & 0x5 or a & 0x1000 or not ok(f["type"]):  # public|protected, not synthetic
                continue
            fm = ["public" if a & 1 else "protected"]
            if a & 0x8:
                fm.append("static")
            val = f.get("value")
            if a & 0x10 and a & 0x8 and val is not None and (base(f["type"]) in PRIM or f["type"] == "Ljava/lang/String;"):
                fm.append("final")
                t = f["type"]
                if t == "J":
                    val += "L"
                elif t == "F":
                    val = val + "f" if val not in ("NaN", "Infinity", "-Infinity") else "0f"
                elif t == "D" and val in ("NaN", "Infinity", "-Infinity"):
                    val = "0d"
                elif t == "C":
                    val = f"(char) {int(val)}" if val.lstrip("-").isdigit() else val
                elif t == "Z":
                    val = "true" if val in ("true", "1") else "false"
                elif t == "B":
                    val = f"(byte) {val}"
                elif t == "S":
                    val = f"(short) {val}"
                lines.append(f"{inner}{' '.join(fm)} {java(t)} {f['name']} = {val};")
            elif is_iface:
                lines.append(f"{inner}{java(f['type'])} {f['name']} = {default(f['type'])};")
            else:
                lines.append(f"{inner}{' '.join(fm)} {java(f['type'])} {f['name']};")
        has_noarg = False
        seen = set()
        for m in c["methods"]:
            a = m.get("access", 0)
            name = m.get("name", "")
            if name == "<clinit>" or not a & 0x5 or a & 0x1040:  # synthetic|bridge
                continue
            args, ret = split_types(m["type"])
            if not all(ok(t) for t in args) or not ok(ret):
                continue
            key = (name, tuple(args))
            if key in seen:
                continue
            seen.add(key)
            params = ", ".join(f"{java(t)} a{i}" for i, t in enumerate(args))
            vis = "public" if a & 1 else "protected"
            if name == "<init>":
                if is_iface:
                    continue
                if not args:
                    has_noarg = True
                lines.append(f"{inner}{vis} {simple}({params}) {{ throw new RuntimeException(\"stub\"); }}")
                continue
            mm = [vis]
            if a & 0x8:
                mm.append("static")
            if a & 0x10 and not is_iface:
                mm.append("final")  # so javac rejects overriding final framework methods
            if is_iface and not a & 0x8:
                body = ";" if a & 0x400 else " { throw new RuntimeException(\"stub\"); }"
                if not a & 0x400:
                    mm = ["default"]
                else:
                    mm = []
            elif a & 0x400:
                mm.append("abstract")
                body = ";"
            else:
                body = " { throw new RuntimeException(\"stub\"); }"
            lines.append(f"{inner}{' '.join(mm)} {java(ret)} {name}({params}){body}".replace("  ", " "))
        if not is_iface and not has_noarg:
            # Lets stub subclasses compile without matching super(...) calls.
            lines.append(f"{inner}protected {simple}() {{ throw new RuntimeException(\"stub\"); }}")
        for ch in children[desc]:
            lines.extend(emit(ch, depth + 1, True))
        lines.append(f"{ind}}}")
        return lines

    import os
    for d in tops:
        path = d[1:-1].split("$")[0]
        pkg = "/".join(path.split("/")[:-1])
        f = os.path.join(out_dir, path + ".java")
        os.makedirs(os.path.dirname(f), exist_ok=True)
        with open(f, "w") as fh:
            fh.write(f"// Generated by tools/gen_ims_stubs.py from device framework. Compile-only.\n")
            fh.write(f"package {pkg.replace('/', '.')};\n\n")
            fh.write("\n".join(emit(d, 0, False)) + "\n")
    # javac needs LambdaMetafactory to compile lambdas; android.jar lacks it
    # (D8 desugars them, so it is not needed at run time).
    lm = os.path.join(out_dir, "java/lang/invoke/LambdaMetafactory.java")
    os.makedirs(os.path.dirname(lm), exist_ok=True)
    with open(lm, "w") as fh:
        fh.write("package java.lang.invoke;\n\npublic final class LambdaMetafactory {\n"
                 "    public static CallSite metafactory(MethodHandles.Lookup a, String b, MethodType c, MethodType d,"
                 " MethodHandle e, MethodType f) throws LambdaConversionException { throw new RuntimeException(\"stub\"); }\n"
                 "    public static CallSite altMetafactory(MethodHandles.Lookup a, String b, MethodType c, Object... d)"
                 " throws LambdaConversionException { throw new RuntimeException(\"stub\"); }\n}\n")
    print(f"{len(tops)} top-level classes, {len(kept)} total")


def default(t):
    b = base(t)
    if t != b or b not in PRIM:
        return "null"
    return {"Z": "false", "J": "0L", "F": "0f", "D": "0d"}.get(b, "0")


if __name__ == "__main__":
    main()
