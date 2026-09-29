"""Release-tool tests: source definitions with fake git/go/gh, never release.sh.

No test creates real Git tags, pushes refs, contacts GitHub, or calls a provider.
"""

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO = Path(__file__).resolve().parents[2]
SHA = "a" * 40
OLDER = "b" * 40
BASE = "github.com/emotional-data8482/automata"

# Every potentially publishing command is intercepted in an isolated PATH.
FAKE_COMMAND = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
state_path = Path(os.environ["FAKE_STATE"])
state = json.loads(state_path.read_text())
entry = {"command": command, "args": args, "cwd": os.getcwd(),
         "work": os.environ.get("GOWORK"), "cache": os.environ.get("GOMODCACHE"),
         "proxy": os.environ.get("GOPROXY"), "sumdb": os.environ.get("GOSUMDB")}
if command == "gh" and args[:2] == ["release", "create"]:
    entry["notes"] = Path(args[args.index("--notes-file") + 1]).read_text()
if command == "go" and args[:1] == ["build"] and Path("main.go").exists():
    entry["source"] = Path("main.go").read_text()
with open(os.environ["FAKE_LOG"], "a") as log:
    log.write(json.dumps(entry) + "\n")

def save():
    state_path.write_text(json.dumps(state))

def stop(code=0, text=""):
    if text:
        print(text)
    sys.exit(code)

if command == "git":
    if args[:1] == ["-C"]:
        args = args[2:]
    if args == ["rev-parse", "HEAD"]:
        stop(text=state["head"])
    if args[:1] == ["status"]:
        stop(state.get("status_error", 0), state.get("dirty", ""))
    if args[:2] == ["merge-base", "--is-ancestor"]:
        stop(0 if state.get("ancestor", True) else 1)
    if args == ["tag", "--list"]:
        stop(text="\n".join(state["tags"]))
    if args[:2] == ["rev-parse", "--verify"]:
        tag = args[2].removeprefix("refs/tags/").removesuffix("^{commit}")
        stop(0 if tag in state["tags"] else 128, state["tags"].get(tag, ""))
    if args[:1] == ["log"]:
        stop(text="- fix: module behavior (123abcd)")
    if args[:1] == ["ls-remote"]:
        if state.get("remote_error"):
            stop(128)
        if not state.get("remote_sha"):
            stop(2)
        ref = args[3]
        if state.get("annotated"):
            stop(text="c" * 40 + "\t" + ref + "\n" + state["remote_sha"] + "\t" + ref + "^{}")
        stop(text=state["remote_sha"] + "\t" + ref)
    if args[:1] == ["push"]:
        state["remote_sha"] = args[2].split(":")[0]
        save()
        stop()
    if args[:1] == ["diff"]:
        stop()
elif command == "gh":
    if args[:1] == ["api"]:
        stop(text="true" if state.get("protected", True) else "false")
    if args[:2] == ["release", "view"]:
        stop(0 if state.get("release_exists") else 1)
    if args[:2] == ["release", "create"]:
        if state.get("create_error"):
            stop(1)
        state["release_exists"] = True
        save()
        stop()
elif command == "go":
    if args == ["mod", "edit", "-json"]:
        relative = os.path.relpath(os.getcwd(), os.environ["FAKE_ROOT"])
        info = {"Module": {"Path": os.environ["FAKE_BASE"] + ("/" + relative if relative != "." else "")}}
        info.update(state.get("mod_info", {}).get(relative, {}))
        stop(text=json.dumps(info))
    if args == ["work", "edit", "-json"]:
        stop(text=json.dumps({"Use": [{"DiskPath": p} for p in state["work_paths"]]}))
    if args == ["mod", "tidy", "-diff"]:
        stop(1 if state.get("untidy") else 0)
    if args[:1] == ["get"]:
        if state.get("readonly_cache"):
            directory = Path(os.environ["GOMODCACHE"], "module@version")
            directory.mkdir(parents=True, exist_ok=True)
            (directory / "source.go").write_text("read-only downloaded source\n")
            (directory / "source.go").chmod(0o400)
            directory.chmod(0o500)
        if state.get("get_failures", 0):
            state["get_failures"] -= 1
            save()
            stop(1)
        stop()
    if args[:3] == ["list", "-m", "-json"]:
        result = {"Path": args[3], "Version": state.get("consumer_version", "v0.5.3")}
        if state.get("consumer_replace"):
            result["Replace"] = {"Path": "../local"}
        stop(text=json.dumps(result))
    if args[:1] in (["test"], ["vet"], ["build"], ["list"]):
        if state.get("change_metadata") and args[:1] == ["test"]:
            Path(os.environ["FAKE_ROOT"], "go.work").write_text("changed during test\n")
        stop()
    if args[:2] == ["mod", "init"]:
        Path("go.mod").write_text("module automata-release-smoke\n")
        stop()
elif command == "sleep":
    stop()
stop(99, "unexpected fake command: " + command + " " + repr(args))
'''


class ReleaseToolsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name).resolve()
        self.root = self.directory / "checkout"
        scripts = self.root / "scripts"
        scripts.mkdir(parents=True)
        for name in ("release-lib.sh", "check-modules.sh", "modules.json"):
            shutil.copy2(REPO / "scripts" / name, scripts / name)
        inventory = json.loads((scripts / "modules.json").read_text())
        self.inventory = inventory
        (self.root / "go.work").write_text("go 1.26.2\n")
        for module in inventory["modules"]:
            path = self.root / module["path"]
            path.mkdir(exist_ok=True, parents=True)
            (path / "go.mod").write_text("module fixture\n")
        self.state_path = self.directory / "state.json"
        self.log_path = self.directory / "commands.jsonl"
        self.state = {
            "head": SHA,
            "tags": {"v0.5.2": OLDER, "tools/v0.5.2": OLDER,
                     "extensions/openai/v0.5.2": OLDER},
            "work_paths": [m["path"] for m in inventory["modules"]],
        }
        self.save()
        binary = self.directory / "bin"
        binary.mkdir()
        for name in ("git", "go", "gh", "sleep"):
            executable = binary / name
            executable.write_text(FAKE_COMMAND)
            executable.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{binary}:{os.environ['PATH']}",
                        FAKE_STATE=str(self.state_path), FAKE_LOG=str(self.log_path),
                        FAKE_ROOT=str(self.root), FAKE_BASE=BASE,
                        GITHUB_ACTIONS="true", AUTOMATA_RELEASE_APPROVED="true",
                        GITHUB_REF="refs/heads/main", RELEASE_DEFAULT_BRANCH="main",
                        GITHUB_REPOSITORY="emotional-data8482/automata")
        self.env.pop("GH_TOKEN", None)
        self.env.pop("GITHUB_TOKEN", None)

    def save(self):
        self.state_path.write_text(json.dumps(self.state))

    def commands(self, command=None):
        entries = [json.loads(line) for line in self.log_path.read_text().splitlines()] if self.log_path.exists() else []
        return [e for e in entries if command is None or e["command"] == command]

    def definition(self, function, *args):
        return subprocess.run(
            ["bash", "-c", 'set -euo pipefail; source "$1"; shift; "$@"', "test",
             str(self.root / "scripts/release-lib.sh"), function, *args],
            env=self.env, text=True, capture_output=True)

    def check(self, *args):
        return subprocess.run(["bash", str(self.root / "scripts/check-modules.sh"), *args],
                              env=self.env, text=True, capture_output=True)

    def assert_success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_no_publication(self):
        self.assertFalse([e for e in self.commands("git") if "push" in e["args"]])
        self.assertFalse([e for e in self.commands("gh") if e["args"][:2] == ["release", "create"]])

    def test_inventory_and_matrix(self):
        self.assert_success(self.check("inventory"))
        matrix = json.loads(self.check("matrix").stdout)
        self.assertEqual(len(matrix), len(self.inventory["modules"]))
        race_paths = [m["path"] for m in matrix if m["race"]]
        self.assertEqual(race_paths, [m["path"] for m in self.inventory["modules"] if m["race"]])
        self.assertIn(".", race_paths)
        self.assertIn("extensions/sqlite", race_paths)

    def test_inventory_must_match_workspace(self):
        self.state["work_paths"].pop()
        self.save()
        result = self.check("inventory")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("inventory differ", result.stderr)

    def test_module_tag_names(self):
        for module, tag in ((".", "v0.5.3"), ("tools", "tools/v0.5.3"),
                            ("extensions/openai", "extensions/openai/v0.5.3")):
            with self.subTest(module=module):
                result = self.definition("release_plan", module, "v0.5.3", SHA)
                self.assert_success(result)
                plan = json.loads(result.stdout)
                self.assertEqual(plan["tag"], tag)
                self.assertEqual(plan["commit"], SHA)
        self.assert_no_publication()

    def test_only_exact_supported_stable_versions(self):
        for version in ("patch", "v0.05.3", "v0.5.3-rc.1", "v2.0.0", "v0.5.3; echo BAD"):
            with self.subTest(version=version):
                result = self.definition("release_plan", "tools", version, SHA)
                self.assertNotEqual(result.returncode, 0)
        self.assert_no_publication()

    def test_unknown_modules_and_examples_cannot_release(self):
        for module in ("../tools", "missing", "examples/openai"):
            self.assertNotEqual(self.definition("release_plan", module, "v0.5.3", SHA).returncode, 0)
        self.assert_no_publication()

    def test_versions_are_module_scoped_not_root_scoped(self):
        self.state["tags"]["v1.4.0"] = OLDER
        self.state["tags"]["extensions/claude/v1.8.0"] = OLDER
        self.save()
        self.assert_success(self.definition("release_plan", "tools", "v0.5.3", SHA))
        self.assertNotEqual(self.definition("release_plan", "tools", "v0.5.1", SHA).returncode, 0)

    def test_matching_existing_tag_is_idempotent_even_after_a_later_release(self):
        self.state["tags"].update({"tools/v0.5.3": SHA, "tools/v0.5.4": OLDER})
        self.save()
        result = self.definition("release_plan", "tools", "v0.5.3", SHA)
        self.assert_success(result)
        self.assertEqual(json.loads(result.stdout)["previous_tag"], "tools/v0.5.2")

    def test_conflicting_tag_is_rejected(self):
        self.state["tags"]["tools/v0.5.3"] = OLDER
        self.save()
        result = self.definition("release_plan", "tools", "v0.5.3", SHA)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("another commit", result.stderr)
        self.assert_no_publication()

    def test_dirty_unmerged_and_mismatched_checkouts_are_rejected(self):
        for field, value in (("dirty", "?? secret.env"), ("ancestor", False), ("head", OLDER)):
            with self.subTest(field=field):
                original = dict(self.state)
                self.state[field] = value
                self.save()
                self.assertNotEqual(self.definition("release_plan", "tools", "v0.5.3", SHA).returncode, 0)
                self.state = original
        self.assert_no_publication()

    def test_status_lookup_errors_fail_closed(self):
        self.state["status_error"] = 128
        self.save()
        self.assertNotEqual(self.definition("release_plan", "tools", "v0.5.3", SHA).returncode, 0)
        self.assert_no_publication()

    def test_module_path_must_match_inventory(self):
        self.state["mod_info"] = {"tools": {"Module": {"Path": "example.com/other"}}}
        self.save()
        self.assertNotEqual(self.definition("release_plan", "tools", "v0.5.3", SHA).returncode, 0)

    def test_publication_requires_ci_default_branch_canonical_repo_and_approval(self):
        for field, value in (("GITHUB_ACTIONS", "false"), ("AUTOMATA_RELEASE_APPROVED", "false"),
                             ("GITHUB_REF", "refs/heads/feature"), ("GITHUB_REPOSITORY", "fork/automata")):
            with self.subTest(field=field):
                original = self.env[field]
                self.env[field] = value
                self.assertNotEqual(self.definition("publish_release", "tools", "v0.5.3", SHA).returncode, 0)
                self.env[field] = original
        self.assert_no_publication()

    def test_missing_required_reviewer_fails_closed(self):
        self.state["protected"] = False
        self.save()
        result = self.definition("publish_release", "tools", "v0.5.3", SHA)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("required reviewer", result.stderr)
        self.assert_no_publication()

    def test_publication_pushes_only_one_tag_and_creates_verified_release(self):
        before = {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file()}
        result = self.definition("publish_release", "tools", "v0.5.3", SHA)
        self.assert_success(result)
        push = [e for e in self.commands("git") if "push" in e["args"]]
        self.assertEqual(len(push), 1)
        self.assertEqual(push[0]["args"][-3:], ["push", "origin", f"{SHA}:refs/tags/tools/v0.5.3"])
        create = [e for e in self.commands("gh") if e["args"][:2] == ["release", "create"]]
        self.assertEqual(len(create), 1)
        self.assertIn("--verify-tag", create[0]["args"])
        self.assertIn("--latest=false", create[0]["args"])
        self.assertIn(f"go get {BASE}/tools@v0.5.3", create[0]["notes"])
        self.assertEqual(before, {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file()})

    def test_latest_label_is_reserved_for_root_releases(self):
        self.assert_success(self.definition("publish_release", ".", "v0.5.3", SHA))
        create = next(e for e in self.commands("gh") if e["args"][:2] == ["release", "create"])
        self.assertIn("--latest=true", create["args"])

    def test_older_root_partial_release_does_not_become_latest(self):
        self.state["tags"].update({"v0.5.3": SHA, "v0.5.4": OLDER})
        self.state["remote_sha"] = SHA
        self.save()
        self.assert_success(self.definition("publish_release", ".", "v0.5.3", SHA))
        create = next(e for e in self.commands("gh") if e["args"][:2] == ["release", "create"])
        self.assertIn("--latest=false", create["args"])

    def test_partial_publication_can_be_completed_without_repushing(self):
        self.state["create_error"] = True
        self.save()
        self.assertNotEqual(self.definition("publish_release", "tools", "v0.5.3", SHA).returncode, 0)
        self.state = json.loads(self.state_path.read_text())
        self.state["create_error"] = False
        self.save()
        self.assert_success(self.definition("publish_release", "tools", "v0.5.3", SHA))
        self.assert_success(self.definition("publish_release", "tools", "v0.5.3", SHA))
        self.assertEqual(len([e for e in self.commands("git") if "push" in e["args"]]), 1)
        self.assertEqual(len([e for e in self.commands("gh") if e["args"][:2] == ["release", "create"]]), 2)

    def test_remote_tag_conflicts_and_lookup_errors_do_not_publish(self):
        for field, value in (("remote_sha", OLDER), ("remote_error", True)):
            with self.subTest(field=field):
                original = dict(self.state)
                self.state[field] = value
                self.save()
                self.assertNotEqual(self.definition("publish_release", "tools", "v0.5.3", SHA).returncode, 0)
                self.state = original
        self.assert_no_publication()

    def test_matching_annotated_tag_is_not_recreated(self):
        self.state.update(remote_sha=SHA, annotated=True)
        self.save()
        self.assert_success(self.definition("publish_release", "tools", "v0.5.3", SHA))
        self.assertFalse([e for e in self.commands("git") if "push" in e["args"]])

    def test_root_notes_exclude_nested_module_commits(self):
        self.assert_success(self.definition("publish_release", ".", "v0.5.3", SHA))
        log = next(e for e in self.commands("git") if "log" in e["args"])
        self.assertIn(":(exclude)tools", log["args"])
        self.assertIn(":(exclude)extensions/openai", log["args"])
        self.assertIn(":(exclude)examples/openai", log["args"])

    def test_release_check_rejects_replacements_exclusions_and_untidy_files(self):
        for field in ("Replace", "Exclude"):
            with self.subTest(field=field):
                self.state["mod_info"] = {"tools": {field: [{"Old": {"Path": BASE}}]}}
                self.save()
                self.assertNotEqual(self.check("release", "tools").returncode, 0)
        self.state["mod_info"] = {}
        self.state["untidy"] = True
        self.save()
        self.assertNotEqual(self.check("release", "tools").returncode, 0)
        self.assertFalse([e for e in self.commands("go") if e["args"][:1] == ["test"]])

    def test_isolated_checks_and_root_race_tests(self):
        self.assert_success(self.check("release", "."))
        tests = [e for e in self.commands("go") if e["args"][:1] == ["test"]]
        self.assertEqual(len(tests), 2)
        self.assertTrue(all(e["work"] == "off" for e in tests))
        self.assertIn("-race", tests[1]["args"])

    def test_example_build_output_is_outside_checkout(self):
        self.assert_success(self.check("workspace", "examples/openai"))
        build = next(e for e in self.commands("go") if e["args"][:1] == ["build"])
        output = build["args"][build["args"].index("-o") + 1]
        self.assertFalse(output.startswith(str(self.root)))
        self.assertEqual(build["work"], str(self.root / "go.work"))

    def test_workspace_allows_existing_edits_but_detects_metadata_mutation(self):
        (self.root / "go.work").write_text("preexisting development edits\n")
        self.assert_success(self.check("workspace", "tools"))
        self.state["change_metadata"] = True
        self.save()
        result = self.check("workspace", "tools")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("metadata changed", result.stderr)

    def test_consumer_imports_inventory_subpackage(self):
        self.assert_success(self.check("consumer", "extensions/mcp", "v0.5.3"))
        build = next(e for e in self.commands("go") if e["args"][:1] == ["build"])
        self.assertIn(f'import _ "{BASE}/extensions/mcp/mcpclient"', build["source"])
        self.assert_no_publication()

    def test_consumer_uses_fresh_cache_proxy_checksums_and_exact_version(self):
        self.state["get_failures"] = 2
        self.save()
        self.assert_success(self.check("consumer", ".", "v0.5.3"))
        gets = [e for e in self.commands("go") if e["args"][:1] == ["get"]]
        self.assertEqual(len(gets), 3)
        for entry in gets:
            self.assertEqual(entry["work"], "off")
            self.assertEqual(entry["proxy"], "https://proxy.golang.org")
            self.assertEqual(entry["sumdb"], "sum.golang.org")
            self.assertEqual(Path(entry["cache"]).resolve(), Path(entry["cwd"]) / "modcache")
            self.assertFalse(entry["cwd"].startswith(str(self.root)))
        build = next(e for e in self.commands("go") if e["args"][:1] == ["build"])
        self.assertIn(f'import _ "{BASE}/core"', build["source"])
        self.assert_no_publication()

    def test_consumer_removes_its_readonly_module_cache(self):
        self.state["readonly_cache"] = True
        self.save()
        self.assert_success(self.check("consumer", "tools", "v0.5.3"))
        get = next(e for e in self.commands("go") if e["args"][:1] == ["get"])
        self.assertFalse(Path(get["cache"]).exists())

    def test_consumer_retries_are_bounded(self):
        self.state["get_failures"] = 100
        self.save()
        self.assertNotEqual(self.check("consumer", "tools", "v0.5.3").returncode, 0)
        self.assertEqual(len([e for e in self.commands("go") if e["args"][:1] == ["get"]]), 6)
        self.assertEqual(len(self.commands("sleep")), 5)
        self.assert_no_publication()

    def test_consumer_rejects_wrong_version_or_local_replacement(self):
        for field, value in (("consumer_version", "v0.5.4"), ("consumer_replace", True)):
            with self.subTest(field=field):
                original = dict(self.state)
                self.state[field] = value
                self.save()
                self.assertNotEqual(self.check("consumer", "tools", "v0.5.3").returncode, 0)
                self.state = original
        self.assertFalse([e for e in self.commands("go") if e["args"][:1] == ["build"]])


if __name__ == "__main__":
    unittest.main()
