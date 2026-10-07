"""Verify source-only staging and rejection of unpublished local dependencies."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from prepare_build import MODULE, prepare


class PrepareBuildTest(unittest.TestCase):
    def test_pinned_source_without_secrets_or_source_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root = base / "repo"
            root.mkdir()
            (root / "go.mod").write_text("module example\n")
            (root / ".env").write_text("PRIVATE=value\n")
            (root / ".env.example").write_text("PRIVATE=placeholder\n")
            module = base / "module"
            module.mkdir()
            (module / "go.mod").write_text(f"module {MODULE}\n")
            metadata = {"Path": MODULE, "Version": "v0.0.0-test", "Dir": str(module), "Sum": "h1:test"}
            with patch("prepare_build.subprocess.check_output", side_effect=[
                    json.dumps(metadata), b"go.mod\0.env\0.env.example\0deleted.go\0"]), \
                    patch("prepare_build.subprocess.run") as edit:
                output = prepare(base / "build", root)
            self.assertFalse((output / ".env").exists())
            self.assertTrue((output / ".env.example").exists())
            self.assertTrue((output / ".go-deps/2ai/go.mod").exists())
            self.assertEqual((root / "go.mod").read_text(), "module example\n")
            self.assertEqual(json.loads((output / ".go-deps/source.json").read_text())["Version"], metadata["Version"])
            edit.assert_called_once_with(["go", "mod", "edit", f"-replace={MODULE}=./.go-deps/2ai"], cwd=output, check=True)

    def test_rejects_local_replace_and_existing_output(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            root = base / "repo"
            root.mkdir()
            metadata = {"Path": MODULE, "Replace": {"Dir": "../2ai"}}
            with patch("prepare_build.subprocess.check_output", return_value=json.dumps(metadata)):
                with self.assertRaisesRegex(ValueError, "pinned"):
                    prepare(base / "build", root)
            self.assertFalse((base / "build").exists())
            with self.assertRaisesRegex(ValueError, "new directory"):
                prepare(root, root)


if __name__ == "__main__":
    unittest.main()
