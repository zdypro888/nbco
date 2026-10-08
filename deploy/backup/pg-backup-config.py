import configparser
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from urllib.parse import parse_qsl, unquote, urlsplit


def main():
    os.umask(0o077)
    with open(sys.argv[1], encoding="utf-8") as source:
        dsn = json.load(source)["postgres_dsn"]
    uri = urlsplit(dsn)
    if uri.scheme not in ("postgres", "postgresql") or not uri.path.strip("/"):
        raise ValueError("backup config requires a PostgreSQL URI with a database name")
    parameters = dict(parse_qsl(uri.query, keep_blank_values=True))
    parameters["dbname"] = unquote(uri.path[1:])
    if uri.hostname:
        parameters["host"] = unquote(uri.hostname)
    if uri.port:
        parameters["port"] = str(uri.port)
    if uri.username is not None:
        parameters["user"] = unquote(uri.username)
    if uri.password is not None:
        parameters["password"] = unquote(uri.password)
    if any("\n" in key + value or "\r" in key + value for key, value in parameters.items()):
        raise ValueError("PostgreSQL URI contains invalid service-file values")
    service = configparser.ConfigParser(interpolation=None)
    service["nbco_backup"] = parameters
    with tempfile.TemporaryDirectory(prefix="nbco-pgbackup-") as directory:
        service_file = Path(directory) / "pg_service.conf"
        with service_file.open("w", encoding="utf-8") as output:
            service.write(output, space_around_delimiters=False)
        env = os.environ.copy()
        env.pop("PGDATABASE", None)
        env.update(PGSERVICE="nbco_backup", PGSERVICEFILE=str(service_file))
        subprocess.run(
            ["/bin/sh", str(Path(__file__).with_name("pg-backup.sh")), "-", *sys.argv[2:]],
            env=env,
            check=True,
        )


if __name__ == "__main__":
    main()
