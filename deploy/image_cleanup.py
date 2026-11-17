#!/usr/bin/env python3
"""Retain current images, two rollback versions and the latest build stages."""

import argparse
import datetime
import json
import re
import subprocess
import sys


STAGE_LABEL = "io.bronx.yunpan.build-stage"
STAGES = {"go", "parser-go", "node"}
REPOSITORY = re.compile(r"bx-yunpan-[a-z0-9][a-z0-9-]*\Z")


class Docker:
    def run(self, *args):
        return subprocess.check_output(["docker", *args], text=True).strip()

    def images(self):
        # Inspect visible version images plus our cached stages, rather than every
        # historical intermediate layer from unrelated builds on this daemon.
        ids = set(self.run("image", "ls", "--quiet", "--no-trunc").split())
        ids.update(self.run("image", "ls", "--all", "--quiet", "--no-trunc",
                            "--filter", f"label={STAGE_LABEL}").split())
        ids = sorted(ids)
        images = []
        for offset in range(0, len(ids), 64):
            images.extend(json.loads(self.run("image", "inspect", *ids[offset:offset + 64])))
        return images

    def inspect(self, reference):
        return json.loads(self.run("image", "inspect", reference))[0]

    def references(self):
        # Includes stopped and created containers, not just the running stack.
        ids = self.run("container", "ls", "--all", "--quiet").split()
        references = set()
        for offset in range(0, len(ids), 64):
            references.update(self.run(
                "container", "inspect", "--format", "{{.Image}}", *ids[offset:offset + 64]
            ).split())
        return references


def tags(image):
    return set(image.get("RepoTags") or []) - {"<none>:<none>"}


def stage(image):
    return (image.get("Config", {}).get("Labels") or {}).get(STAGE_LABEL)


def project_image(image):
    names = tags(image)
    return bool(names) and all(REPOSITORY.fullmatch(name.rsplit(":", 1)[0]) for name in names)


def foreign_digest(image):
    # The containerd image store also gives locally built images a RepoDigest.
    return any(not REPOSITORY.fullmatch(name.split("@", 1)[0])
               for name in image.get("RepoDigests") or [])


def created(image):
    return datetime.datetime.fromisoformat(image["Created"].replace("Z", "+00:00"))


def plan(images, references, current_tag):
    keep = set(references)
    repositories = {}
    stages = {}
    for image in images:
        image_id = image["Id"]
        names = tags(image)
        # Mixed repositories and digests outside our local namespace are not ours.
        if foreign_digest(image) or (names and not project_image(image)):
            keep.add(image_id)
            continue
        for repository in {name.rsplit(":", 1)[0] for name in names}:
            repositories.setdefault(repository, {})[image_id] = image
        if not names and stage(image) in STAGES:
            stages.setdefault(stage(image), []).append(image)

    for repository, versions in repositories.items():
        ordered = sorted(versions.values(), key=lambda image: (created(image), image["Id"]), reverse=True)
        current = {image["Id"] for image in ordered if f"{repository}:{current_tag}" in tags(image)}
        # An unbuilt/retired role still retains its newest version plus two older ones.
        current = current or {ordered[0]["Id"]}
        keep.update(current)
        rollbacks = []
        for slot in (1, 2):
            rollbacks.extend(image["Id"] for image in ordered
                             if f"{repository}:build-rollback-{slot}" in tags(image)
                             and image["Id"] not in current and image["Id"] not in rollbacks)
        rollbacks.extend(image["Id"] for image in ordered
                         if image["Id"] not in current and image["Id"] not in rollbacks)
        keep.update(rollbacks[:2])

    for versions in stages.values():
        keep.add(max(versions, key=lambda image: (created(image), image["Id"]))["Id"])

    # --all includes cached intermediate layers; preserve the ancestry of kept images.
    by_id = {image["Id"]: image for image in images}
    pending = list(keep)
    while pending:
        parent = by_id.get(pending.pop(), {}).get("Parent")
        if parent and parent not in keep:
            keep.add(parent)
            pending.append(parent)

    return sorted([
        image for image in images
        if image["Id"] not in keep
        and not foreign_digest(image)
        and (project_image(image) or (not tags(image) and stage(image) in STAGES))
    ], key=lambda image: (created(image), image["Id"]), reverse=True)


def protect(docker, targets, current_tag):
    by_tag = {name: image for image in docker.images() for name in tags(image)}
    for target in targets:
        reference = f"bx-yunpan-{target}:{current_tag}"
        image = by_tag.get(reference)
        if image is None:
            continue
        # Pin by immutable ID before overwriting a mutable tag, even without a container.
        rollback = f"bx-yunpan-{target}:build-rollback-{image['Id'].removeprefix('sha256:')}"
        existing = by_tag.get(rollback)
        if existing is not None and existing["Id"] != image["Id"]:
            raise RuntimeError(f"Rollback tag unexpectedly points to another image: {rollback}")
        docker.run("image", "tag", image["Id"], rollback)
        previous = by_tag.get(f"bx-yunpan-{target}:build-rollback-1")
        if previous is not None and previous["Id"] != image["Id"]:
            docker.run("image", "tag", previous["Id"], f"bx-yunpan-{target}:build-rollback-2")
        docker.run("image", "tag", image["Id"], f"bx-yunpan-{target}:build-rollback-1")
        print(f"Preserved rollback: {rollback}", flush=True)


def cleanup(docker, current_tag, dry_run=False):
    candidates = plan(docker.images(), docker.references(), current_tag)
    removed = 0
    failed = False
    for candidate in candidates:
        image_id = candidate["Id"]
        names = tags(candidate)
        if dry_run:
            print(f"Would remove: {image_id} {' '.join(sorted(names)) or '(project build stage)'}")
            continue
        # Recheck ownership, tags and all container references immediately before removal.
        fresh = docker.inspect(image_id)
        if tags(fresh) != names or foreign_digest(fresh) or image_id in docker.references():
            print(f"Skipped changed or referenced image: {image_id}", flush=True)
            continue
        if names:
            if not project_image(fresh) or any(name.rsplit(":", 1)[1] == current_tag for name in names):
                continue
            if any(docker.inspect(name)["Id"] != image_id for name in names):
                print(f"Skipped retagged image: {image_id}", flush=True)
                continue
            arguments = sorted(names)
        else:
            if stage(fresh) not in STAGES:
                continue
            # Stage ancestors are explicit candidates too; remove them one at a time
            # so a child removal cannot implicitly delete a later candidate.
            arguments = ["--no-prune", image_id]
        try:
            # Docker also refuses to remove referenced images/parents; never force or prune globally.
            output = docker.run("image", "rm", *arguments)
            if output:
                print(output, flush=True)
            removed += 1
        except subprocess.CalledProcessError:
            failed = True
            print(f"Could not remove image (possibly newly referenced): {image_id}", file=sys.stderr)
    print(f"Image cleanup: {len(candidates)} candidates, {removed} removed; "
          "retained current images, two rollback versions per role and latest project build stages.")
    return 1 if failed else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    protect_parser = commands.add_parser("protect", help="pin existing target images before a build")
    protect_parser.add_argument("--tag", required=True)
    protect_parser.add_argument("targets", nargs="+")
    cleanup_parser = commands.add_parser("cleanup", help="remove obsolete project images")
    cleanup_parser.add_argument("--tag", required=True)
    cleanup_parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    docker = Docker()
    try:
        if args.command == "protect":
            protect(docker, args.targets, args.tag)
            return 0
        return cleanup(docker, args.tag, args.dry_run)
    except subprocess.CalledProcessError as error:
        print(f"Image cleanup stopped: Docker command failed with exit code {error.returncode}.", file=sys.stderr)
        return 1
    except (ValueError, KeyError, RuntimeError) as error:
        print(f"Image cleanup stopped: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
