import pathlib
import subprocess
import tempfile
import unittest


class UpgradeBinaryLinkTest(unittest.TestCase):
    def run_link(self, current, link):
        script = pathlib.Path(__file__).resolve().parents[2] / "scripts/upgrade-nbco.sh"
        source = script.read_text().removesuffix('main "$@"\n')
        self.assertNotEqual(source, script.read_text())
        subprocess.run(
            ["bash", "-s", "--", str(current), str(link)],
            input=source + '\ncurrent_bin="$1"\nlink_bin="$2"\nrefresh_binary_link\n',
            text=True,
            check=True,
            capture_output=True,
        )

    def test_direct_binary_remains_regular_file(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = pathlib.Path(directory) / "nbco"
            binary.write_bytes(b"binary")
            self.run_link(binary, binary)
            self.assertFalse(binary.is_symlink())
            self.assertEqual(binary.read_bytes(), b"binary")

    def test_separate_link_points_to_installed_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = pathlib.Path(directory) / "nbco-linux"
            binary.write_bytes(b"binary")
            link = pathlib.Path(directory) / "nbco"
            self.run_link(binary, link)
            self.assertTrue(link.is_symlink())
            self.assertEqual(link.resolve(), binary.resolve())

    def test_existing_link_is_refreshed_for_install_and_rollback(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            old, new, link = root / "old", root / "new", root / "nbco"
            old.write_bytes(b"old")
            new.write_bytes(b"new")
            link.symlink_to(old)
            self.run_link(new, link)
            self.assertEqual(link.read_bytes(), b"new")
            self.run_link(old, link)
            self.assertEqual(link.read_bytes(), b"old")
