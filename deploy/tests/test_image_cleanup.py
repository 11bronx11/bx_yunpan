import contextlib
import copy
import io
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


DEPLOY = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(DEPLOY))
import image_cleanup as retention


def image(number, *names, stage=None, digests=None):
    return {
        "Id": f"sha256:{number:064x}",
        "Created": f"2026-11-{number:02d}T00:00:00Z",
        "RepoTags": list(names),
        "RepoDigests": digests or [],
        "Config": {"Labels": {retention.STAGE_LABEL: stage} if stage else {}},
    }


class FakeDocker:
    def __init__(self, images, references=()):
        self.inventory = copy.deepcopy(images)
        self.used = set(references)
        self.commands = []
        self.reference_reads = 0
        self.new_reference = None
        self.changed_tag = None

    def images(self):
        return copy.deepcopy(self.inventory)

    def references(self):
        self.reference_reads += 1
        if self.reference_reads > 1 and self.new_reference:
            self.used.add(self.new_reference)
        return set(self.used)

    def inspect(self, reference):
        if reference == self.changed_tag:
            return image(20, reference)
        for item in self.inventory:
            if reference == item["Id"] or reference in item["RepoTags"]:
                return copy.deepcopy(item)
        raise subprocess.CalledProcessError(1, ["docker", "image", "inspect", reference])

    def run(self, *arguments):
        self.commands.append(arguments)
        if arguments[:2] == ("image", "tag"):
            for item in self.inventory:
                if arguments[3] in item["RepoTags"]:
                    item["RepoTags"].remove(arguments[3])
            for item in self.inventory:
                if item["Id"] == arguments[2] and arguments[3] not in item["RepoTags"]:
                    item["RepoTags"].append(arguments[3])
        return ""


class ImageRetentionTests(unittest.TestCase):
    def candidates(self, images, references=(), tag="current"):
        return {item["Id"] for item in retention.plan(images, set(references), tag)}

    def versions(self):
        return [image(n, f"bx-yunpan-api:v{n}") for n in range(1, 6)]

    def test_inventory_includes_project_stage_parents_hidden_by_default_listing(self):
        runtime = image(3, "bx-yunpan-api:current")
        cached = image(2, stage="go")
        parent = image(1, stage="go")
        class InventoryDocker(retention.Docker):
            def run(self, *arguments):
                if arguments[:2] == ("image", "ls"):
                    if "--filter" in arguments:
                        return "\n".join([parent["Id"], cached["Id"]])
                    return runtime["Id"]
                self_ids = set(arguments[2:])
                return retention.json.dumps([i for i in [runtime, cached, parent] if i["Id"] in self_ids])
        self.assertEqual({i["Id"] for i in InventoryDocker().images()},
                         {runtime["Id"], cached["Id"], parent["Id"]})

    def test_keeps_current_and_two_distinct_rollback_images_per_role(self):
        versions = self.versions()
        versions[2]["RepoTags"] += ["bx-yunpan-api:before-a", "bx-yunpan-api:before-b"]
        versions[-1]["RepoTags"] += ["bx-yunpan-api:current"]
        web = [image(n, f"bx-yunpan-web:v{n}") for n in range(6, 10)]
        self.assertEqual(self.candidates(versions + web), {
            versions[0]["Id"], versions[1]["Id"], web[0]["Id"],
        })

    def test_explicit_current_is_kept_even_if_older_than_other_builds(self):
        versions = self.versions()
        versions[0]["RepoTags"] += ["bx-yunpan-api:current"]
        self.assertEqual(self.candidates(versions), {versions[1]["Id"], versions[2]["Id"]})

    def test_recently_used_rollbacks_take_priority_over_image_creation_time(self):
        versions = self.versions()
        versions[0]["RepoTags"].append("bx-yunpan-api:build-rollback-1")
        versions[1]["RepoTags"].append("bx-yunpan-api:build-rollback-2")
        versions[-1]["RepoTags"].append("bx-yunpan-api:current")
        self.assertEqual(self.candidates(versions), {versions[2]["Id"], versions[3]["Id"]})

    def test_all_container_references_are_protected(self):
        versions = self.versions()
        self.assertEqual(self.candidates(versions, [versions[0]["Id"]]), {versions[1]["Id"]})

    def test_foreign_tags_digests_and_unlabelled_dangling_images_are_protected(self):
        versions = self.versions()
        versions[0]["RepoTags"].append("other-project:archive")
        versions[1]["RepoDigests"] = ["registry.example/app@sha256:abc"]
        unrelated = [image(6), image(7, "alpine:3.22"), image(8, "some-app:old")]
        self.assertEqual(self.candidates(versions + unrelated), set())

    def test_local_containerd_digests_do_not_disable_retention(self):
        versions = self.versions()
        for item in versions:
            item["RepoDigests"] = [f"bx-yunpan-api@{item['Id']}"]
        self.assertEqual(self.candidates(versions), {versions[0]["Id"], versions[1]["Id"]})

    def test_keeps_latest_project_build_stage_and_referenced_older_stage(self):
        old = image(1, stage="go")
        cached = image(2, stage="go")
        newest = image(3, stage="go")
        node = image(4, stage="node")
        parser = image(5, stage="parser-go")
        unrelated = image(6, stage="unknown")
        self.assertEqual(self.candidates([old, cached, newest, node, parser, unrelated], [old["Id"]]),
                         {cached["Id"]})

    def test_image_kept_by_another_project_role_is_not_deleted(self):
        versions = self.versions()
        versions[0]["RepoTags"].append("bx-yunpan-web:current")
        self.assertEqual(self.candidates(versions), {versions[1]["Id"]})

    def test_ancestry_of_latest_cache_and_other_kept_images_is_protected(self):
        parent = image(1, stage="go")
        latest = image(3, stage="go")
        latest["Parent"] = parent["Id"]
        unused = image(2, stage="go")
        self.assertEqual(self.candidates([parent, latest, unused]), {unused["Id"]})

    def test_protect_pins_immutable_previous_image_before_tag_overwrite(self):
        previous = image(1, "bx-yunpan-api:current")
        docker = FakeDocker([previous])
        with contextlib.redirect_stdout(io.StringIO()):
            retention.protect(docker, ["api", "web"], "current")
        self.assertEqual(docker.commands, [(
            "image", "tag", previous["Id"],
            f"bx-yunpan-api:build-rollback-{previous['Id'].removeprefix('sha256:')}",
        ), ("image", "tag", previous["Id"], "bx-yunpan-api:build-rollback-1")])

    def test_rollback_slots_rotate_once_when_current_image_changes(self):
        older = image(1, "bx-yunpan-api:build-rollback-1")
        previous = image(2, "bx-yunpan-api:current")
        docker = FakeDocker([older, previous])
        with contextlib.redirect_stdout(io.StringIO()):
            retention.protect(docker, ["api"], "current")
            retention.protect(docker, ["api"], "current")
        self.assertEqual(docker.inspect("bx-yunpan-api:build-rollback-1")["Id"], previous["Id"])
        self.assertEqual(docker.inspect("bx-yunpan-api:build-rollback-2")["Id"], older["Id"])
        self.assertEqual(sum(cmd[-1] == "bx-yunpan-api:build-rollback-2" for cmd in docker.commands), 1)

    def test_dry_run_has_no_mutations(self):
        docker = FakeDocker(self.versions())
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(retention.cleanup(docker, "current", dry_run=True), 0)
        self.assertEqual(docker.commands, [])

    def test_new_container_reference_prevents_deletion(self):
        versions = self.versions()
        docker = FakeDocker(versions)
        docker.new_reference = versions[1]["Id"]
        with contextlib.redirect_stdout(io.StringIO()):
            retention.cleanup(docker, "current")
        self.assertEqual(docker.commands, [("image", "rm", "bx-yunpan-api:v1")])

    def test_retagging_after_inventory_prevents_deletion(self):
        docker = FakeDocker(self.versions())
        docker.changed_tag = "bx-yunpan-api:v2"
        with contextlib.redirect_stdout(io.StringIO()):
            retention.cleanup(docker, "current")
        self.assertEqual(docker.commands, [("image", "rm", "bx-yunpan-api:v1")])

    def test_inventory_failure_stops_before_deletion(self):
        docker = FakeDocker(self.versions())
        def fail():
            raise subprocess.CalledProcessError(1, ["docker", "container", "ls"])
        docker.references = fail
        with self.assertRaises(subprocess.CalledProcessError):
            retention.cleanup(docker, "current")
        self.assertEqual(docker.commands, [])


class BuildIntegrationTests(unittest.TestCase):
    def run_build(self, fail_target=""):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            deploy = root / "deploy"
            binaries = root / "bin"
            deploy.mkdir()
            binaries.mkdir()
            for name in ["build.sh", "common.sh"]:
                shutil.copy(DEPLOY / name, deploy / name)
            (deploy / ".env").write_text("APP_IMAGE_TAG=current\n")
            (binaries / "docker").write_text('''#!/bin/sh
printf 'docker %s\\n' "$*" >> "$RETENTION_TEST_LOG"
if [ "$1" = build ] && [ -n "$RETENTION_FAIL_TARGET" ]; then
  case "$*" in *"bx-yunpan-$RETENTION_FAIL_TARGET:current"*) exit 42;; esac
fi
''')
            (binaries / "python3").write_text('''#!/bin/sh
shift
printf 'helper %s\\n' "$*" >> "$RETENTION_TEST_LOG"
''')
            for executable in binaries.iterdir():
                executable.chmod(0o755)
            log = root / "commands"
            env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                       RETENTION_TEST_LOG=str(log), RETENTION_FAIL_TARGET=fail_target,
                       APP_IMAGE_TAG="current", BUILD_MIN_AVAILABLE_MB="1", BUILD_MIN_AVAILABLE_DISK_MB="1")
            result = subprocess.run(["bash", str(deploy / "build.sh"), "api", "web"],
                                    env=env, capture_output=True, text=True)
            return result, log.read_text().splitlines()

    def test_successful_batch_protects_before_build_and_cleans_after_all_builds(self):
        result, commands = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        helper = [command for command in commands if command.startswith("helper ")]
        self.assertEqual(helper, ["helper protect --tag current api web", "helper cleanup --tag current"])
        self.assertLess(commands.index(helper[0]), next(i for i, cmd in enumerate(commands) if cmd.startswith("docker build ")))
        self.assertEqual(commands[-1], helper[-1])

    def test_failed_batch_does_not_clean_any_images(self):
        for target in ["api", "web"]:
            with self.subTest(target=target):
                result, commands = self.run_build(target)
                self.assertEqual(result.returncode, 42, result.stderr)
                self.assertFalse(any(command.startswith("helper cleanup ") for command in commands))


if __name__ == "__main__":
    unittest.main()
