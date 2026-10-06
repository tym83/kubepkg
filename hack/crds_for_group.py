#!/usr/bin/env python3
"""Copy the kubepkg CRDs under another API group.

For platforms that serve the kubepkg types under their own group (the
operator's --api-group). Usage: crds_for_group.py <crd-dir> <out-dir> <group>
"""
import pathlib, sys, yaml

src, dst, group = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
dst.mkdir(parents=True, exist_ok=True)
for f in sorted(src.glob("kubepkg.dev_*.yaml")):
    crd = yaml.safe_load(f.read_text())
    crd["spec"]["group"] = group
    crd["metadata"]["name"] = f'{crd["spec"]["names"]["plural"]}.{group}'
    for v in crd["spec"]["versions"]:
        if "deprecationWarning" in v:
            v["deprecationWarning"] = v["deprecationWarning"].replace("kubepkg.dev/", group + "/")
    out = dst / f.name.replace("kubepkg.dev_", f"{group}_")
    out.write_text("---\n" + yaml.safe_dump(crd, sort_keys=False))
