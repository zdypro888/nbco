# Skill: nbco production upgrade

## Trigger

Use this when a superadmin asks to upgrade, deploy, publish, restart, or self-update the production nbco service on `im.app`.

## Summary

Production upgrades must go through the fixed upgrade script instead of ad hoc shell commands. The script tests, builds, backs up the old binary, restarts `nbco`, checks `/healthz`, and rolls back automatically if the new service does not become healthy.

## Procedure

1. Confirm the requested target ref. If the user did not specify one, use `origin/main`.
2. Use the im.app root worker, not a local worker.
3. Run the fixed production script:

   ```bash
   cd /root/src/nbco
   /root/nbco/bin/upgrade-nbco origin/main
   ```

4. Do not hand-write the deploy sequence unless the script itself is broken.
5. Treat a successful run only as one that ends with the service healthy at `https://im.app:8443/healthz`.
6. If the script reports rollback succeeded, tell the user the upgrade failed but production was restored to the previous binary.
7. If both upgrade and rollback health checks fail, report that production needs manual intervention and include the last `journalctl -u nbco` errors.

## Constraints

- Do not skip tests unless the superadmin explicitly asks for an emergency deploy.
- Do not deploy from a dirty production repo.
- `/root/nbco/bin/upgrade-nbco` is the stable production entrypoint. The repository source is `scripts/upgrade-nbco.sh`.
- Do not restart `nbco-worker` as part of an nbco upgrade unless the worker binary itself changed.
- Do not expose tokens, config secrets, or environment dumps in the progress report.
- During `systemctl restart nbco`, Telegram/HTTP may be unavailable for a few seconds. This is expected. The worker service is separate and continues running.
