# PostgreSQL backups

Linux: adjust the paths in `nbco-pgbackup.service`, install both units in
`/etc/systemd/system/`, then run:

```sh
systemctl daemon-reload
systemctl enable --now nbco-pgbackup.timer
systemctl start nbco-pgbackup.service
systemctl list-timers nbco-pgbackup.timer
```

The wrapper requires Python 3 and reads a PostgreSQL URI from `postgres_dsn` in
the protected application config. A temporary owner-only libpq service file
keeps credentials out of process arguments. Backups run daily at 03:15 UTC with
up to 15 minutes of jitter. Only validated, completed dumps are rotated;
failed dumps do not remove previous backups. Directory and dump permissions
default to owner-only. The default retention is 14 days.

Copy completed dumps to a separate machine or configured backup provider.
Host-local backups alone do not protect against losing the server.

Validate recovery in a temporary database, never against the live database:

```sh
createdb nbco_restore_check
pg_restore --exit-on-error --no-owner --no-privileges -d nbco_restore_check /path/to/backup.dump
psql -d nbco_restore_check -c 'select count(*) from users'
dropdb nbco_restore_check
```

For macOS, use `com.nbco.pgbackup.plist` after adjusting its paths and DSN.
