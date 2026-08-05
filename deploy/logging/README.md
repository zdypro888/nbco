# Persistent production logs

Install the bounded journald policy once on a production host:

```bash
install -d -m 0755 /etc/systemd/journald.conf.d /var/log/journal
install -m 0644 deploy/logging/nbco-journald.conf /etc/systemd/journald.conf.d/nbco.conf
systemd-tmpfiles --create --prefix /var/log/journal
systemctl restart systemd-journald
```

The policy retains up to 30 days while capping persistent storage at 512 MB.
Application logs use Go `slog`, so filter severity from its structured `level`
field when the host journal transport does not map it to native syslog priority:

```bash
journalctl -u nbco -u nbco-worker --since today -o cat | grep -E 'level=(WARN|ERROR)'
```
