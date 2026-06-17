#!/usr/bin/env python3
"""Validate the ecosystem manifests that ship with DataWerx Mesh.

These artifacts — the Backstage catalog entry, the Artifact Hub repository
metadata, and the Artifact Hub annotations on the Helm chart — have no Go test
coverage, and a malformed one fails only later in an external system (Artifact
Hub rejecting the listing, Backstage refusing to register the component). This
catches the structural failures up front in CI.

It checks real failure modes, not style: that every document parses, carries the
keys its consumer requires, and that the YAML-in-a-string chart annotations
(which neither `helm lint` nor a plain YAML parse of Chart.yaml would inspect)
are themselves well-formed and correctly shaped.
"""

import sys

import yaml

REPO_ROOT = __file__.rsplit("/hack/", 1)[0]
CHART = f"{REPO_ROOT}/charts/datawerx-mesh"

errors = []


def err(where, msg):
    errors.append(f"{where}: {msg}")


def load_all(path):
    with open(path) as f:
        return list(yaml.safe_load_all(f))


def load(path):
    with open(path) as f:
        return yaml.safe_load(f)


def validate_backstage():
    where = "examples/backstage/catalog-info.yaml"
    try:
        docs = load_all(f"{REPO_ROOT}/{where}")
    except Exception as e:
        err(where, f"does not parse as YAML: {e}")
        return
    allowed_kinds = {"Component", "API", "Resource", "System", "Group", "User", "Domain"}
    if not docs:
        err(where, "no documents found")
        return
    for i, doc in enumerate(docs):
        if doc is None:
            continue
        tag = f"doc[{i}]"
        if doc.get("apiVersion") != "backstage.io/v1alpha1":
            err(where, f"{tag}: apiVersion must be backstage.io/v1alpha1")
        kind = doc.get("kind")
        if kind not in allowed_kinds:
            err(where, f"{tag}: unknown Backstage kind {kind!r}")
        meta = doc.get("metadata") or {}
        if not meta.get("name"):
            err(where, f"{tag}: metadata.name is required")
        spec = doc.get("spec") or {}
        if not spec.get("owner"):
            err(where, f"{tag} ({kind}): spec.owner is required")
        if kind in {"Component", "API", "Resource"} and not spec.get("type"):
            err(where, f"{tag} ({kind}): spec.type is required")


def validate_artifacthub_repo():
    where = "charts/datawerx-mesh/artifacthub-repo.yml"
    try:
        meta = load(f"{CHART}/artifacthub-repo.yml")
    except Exception as e:
        err(where, f"does not parse as YAML: {e}")
        return
    if "repositoryID" not in meta:
        err(where, "repositoryID key is required (may be empty until claimed)")
    owners = meta.get("owners") or []
    for o in owners:
        if not o.get("name") or not o.get("email"):
            err(where, "each owner needs a name and email")


def validate_chart_annotations():
    where = "charts/datawerx-mesh/Chart.yaml"
    try:
        chart = load(f"{CHART}/Chart.yaml")
    except Exception as e:
        err(where, f"does not parse as YAML: {e}")
        return
    annotations = chart.get("annotations") or {}
    # The Artifact Hub annotations whose values are themselves YAML documents.
    yaml_valued = {
        "artifacthub.io/links": list,
        "artifacthub.io/images": list,
        "artifacthub.io/crds": list,
        "artifacthub.io/maintainers": list,
    }
    for key, want_type in yaml_valued.items():
        if key not in annotations:
            continue
        try:
            parsed = yaml.safe_load(annotations[key])
        except Exception as e:
            err(where, f"annotation {key} is not valid YAML: {e}")
            continue
        if not isinstance(parsed, want_type):
            err(where, f"annotation {key} should decode to a {want_type.__name__}")
            continue
        for item in parsed:
            if not isinstance(item, dict):
                err(where, f"annotation {key} entries must be mappings")
                break

    # Spot-check the most listing-critical entries carry their required keys.
    if "artifacthub.io/images" in annotations:
        for img in yaml.safe_load(annotations["artifacthub.io/images"]) or []:
            if not img.get("name") or not img.get("image"):
                err(where, "each artifacthub.io/images entry needs name and image")
    if "artifacthub.io/links" in annotations:
        for link in yaml.safe_load(annotations["artifacthub.io/links"]) or []:
            if not link.get("name") or not link.get("url"):
                err(where, "each artifacthub.io/links entry needs name and url")


def main():
    validate_backstage()
    validate_artifacthub_repo()
    validate_chart_annotations()

    if errors:
        print("Ecosystem manifest validation FAILED:")
        for e in errors:
            print(f"  - {e}")
        sys.exit(1)
    print("Ecosystem manifests OK (Backstage catalog, Artifact Hub repo + annotations).")


if __name__ == "__main__":
    main()
