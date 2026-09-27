import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import real_site


class RealSiteLauncherTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name).resolve()

    def tearDown(self):
        self.directory.cleanup()

    def source_checkout(self, directory):
        for relative in (Path("backend/go.mod"), Path("backend/cmd/server/main.go"),
                         Path("frontend/package.json"), real_site.PRICING_FALLBACK):
            path = directory / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("test fixture\n", encoding="utf-8")
        return directory

    def test_default_lab_is_inside_quota_watch(self):
        self.assertEqual(real_site.PROJECT_ROOT, Path(real_site.__file__).resolve().parents[1])
        self.assertEqual(real_site.DEFAULT_LAB, real_site.PROJECT_ROOT / "data/real-site")

    def test_monorepo_inference_checks_actual_source(self):
        source = self.source_checkout(self.root / "sub2api")
        with mock.patch.object(real_site, "PROJECT_ROOT", source / "tools/quota-watch"):
            self.assertEqual(real_site.resolve_sub2api_dir(None), source)
            (source / real_site.PRICING_FALLBACK).unlink()
            with self.assertRaisesRegex(RuntimeError, "missing backend/resources"):
                real_site.resolve_sub2api_dir(None)

    def test_standalone_requires_an_explicit_source_checkout(self):
        with mock.patch.object(real_site, "PROJECT_ROOT", self.root / "quota-watch"):
            with self.assertRaisesRegex(RuntimeError, "--sub2api-dir"):
                real_site.resolve_sub2api_dir(None)
        with self.assertRaisesRegex(RuntimeError, "Not a Sub2API source checkout"):
            real_site.resolve_sub2api_dir(self.root / "missing")

    def test_environment_option_and_cli_precedence(self):
        environment_source = self.source_checkout(self.root / "environment-sub2api")
        explicit_source = self.source_checkout(self.root / "explicit-sub2api")
        lab = self.root / "lab"
        with mock.patch.dict(os.environ, {"QUOTA_WATCH_SUB2API_DIR": str(environment_source)}), \
                mock.patch.object(real_site, "prepare") as prepare:
            real_site.main(["--lab-dir", str(lab), "prepare"])
            prepare.assert_called_once_with(lab, environment_source)
            prepare.reset_mock()
            real_site.main(["--lab-dir", str(lab), "--sub2api-dir", str(explicit_source), "prepare"])
            prepare.assert_called_once_with(lab, explicit_source)

    def test_invalid_source_does_not_prepare_a_database(self):
        with mock.patch.object(real_site, "prepare") as prepare:
            with self.assertRaises(SystemExit) as failure:
                real_site.main(["--sub2api-dir", str(self.root / "missing"), "prepare"])
            self.assertEqual(failure.exception.code, 1)
            prepare.assert_not_called()

    def test_up_requires_built_binary_before_preparing_database(self):
        source = self.source_checkout(self.root / "sub2api")
        with mock.patch.object(real_site, "prepare") as prepare:
            with self.assertRaisesRegex(RuntimeError, "Build the embedded Sub2API binary"):
                real_site.up(self.root / "lab", source)
            prepare.assert_not_called()

    def test_prepare_keeps_local_pricing_and_outbound_isolation(self):
        source = self.source_checkout(self.root / "sub2api")
        lab = self.root / "lab"
        pg_bin = self.root / "postgres-bin"
        pg_bin.mkdir()
        (pg_bin / "initdb").touch()
        original_exists = Path.exists

        def exists(path):
            return path == Path("/usr/bin/sandbox-exec") or original_exists(path)

        with mock.patch.object(real_site.sys, "platform", "darwin"), \
                mock.patch.object(real_site, "PG_BIN", pg_bin), \
                mock.patch.object(Path, "exists", exists), \
                mock.patch.object(real_site, "run") as run:
            real_site.prepare(lab, source)
            run.assert_called_once()
        runtime = json.loads((lab / "runtime.yaml").read_text(encoding="utf-8"))
        self.assertEqual(runtime["pricing"]["fallback_file"], str(source / real_site.PRICING_FALLBACK))
        self.assertEqual(runtime["pricing"]["remote_url"], "")
        self.assertEqual(runtime["pricing"]["hash_url"], "")
        self.assertFalse(runtime["token_refresh"]["enabled"])
        profile = (lab / "network.sb").read_text(encoding="utf-8")
        self.assertIn("(deny network-outbound", profile)
        self.assertEqual(profile.count("(remote tcp"), 2)
        self.assertIn('(remote tcp "localhost:15432")', profile)
        self.assertIn('(remote tcp "localhost:16379")', profile)

    def test_down_does_not_require_a_source_checkout(self):
        lab = self.root / "lab"
        with mock.patch.object(real_site, "resolve_sub2api_dir") as resolve, \
                mock.patch.object(real_site, "down") as down:
            real_site.main(["--lab-dir", str(lab), "down"])
            resolve.assert_not_called()
            down.assert_called_once_with(lab)


if __name__ == "__main__":
    unittest.main()
