import configparser
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("backup_config", Path(__file__).with_name("pg-backup-config.py"))
backup_config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(backup_config)


class BackupConfigTest(unittest.TestCase):
    def invoke(self, dsn, runner):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config.json"
            config.write_text(json.dumps({"postgres_dsn": dsn}), encoding="utf-8")
            with patch.object(sys, "argv", ["backup", str(config), directory, "14"]), patch.object(
                backup_config.subprocess, "run", side_effect=runner
            ):
                backup_config.main()

    def test_service_file_is_private_and_credentials_are_not_arguments(self):
        files = []

        def runner(args, *, env, check):
            self.assertTrue(check)
            self.assertNotIn("secret", " ".join(args))
            self.assertNotIn("PGDATABASE", env)
            service_file = Path(env["PGSERVICEFILE"])
            files.append(service_file)
            self.assertEqual(service_file.stat().st_mode & 0o777, 0o600)
            content = service_file.read_text()
            self.assertIn("password=secret%value", content)
            self.assertNotIn(" = ", content)
            service = configparser.ConfigParser(interpolation=None)
            service.read(service_file)
            self.assertEqual(dict(service["nbco_backup"]), {
                "host": "localhost", "port": "5433", "user": "nbco",
                "password": "secret%value", "dbname": "nbco", "sslmode": "require",
            })

        with patch.dict(os.environ, {"PGDATABASE": "unrelated"}):
            self.invoke("postgres://nbco:secret%25value@localhost:5433/nbco?sslmode=require", runner)
        self.assertEqual(len(files), 1)
        self.assertFalse(files[0].exists())

    def test_service_file_removed_after_dump_failure(self):
        files = []

        def runner(args, *, env, check):
            files.append(Path(env["PGSERVICEFILE"]))
            raise subprocess.CalledProcessError(1, args)

        with self.assertRaises(subprocess.CalledProcessError):
            self.invoke("postgres:///nbco", runner)
        self.assertFalse(files[0].exists())

    def test_invalid_uri_rejected_before_dump(self):
        for uri in ["dbname=nbco", "postgres://localhost", "postgres:///nbco?password=a%0Ab"]:
            with self.subTest(uri=uri), self.assertRaises(ValueError):
                self.invoke(uri, lambda *args, **kwargs: self.fail("dump should not run"))


if __name__ == "__main__":
    unittest.main()
