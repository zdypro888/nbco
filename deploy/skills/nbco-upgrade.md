# Skill: nbco production upgrade

## Trigger

Use this when a superadmin asks to upgrade, deploy, publish, restart, or self-update an nbco service.

## Summary

Production upgrades must go through the deployment's configured upgrade entrypoint instead of ad hoc shell commands. The script tests, builds, backs up the old binary, restarts `nbco`, checks `/healthz`, and rolls back automatically if the new service does not become healthy.

## Procedure

1. Confirm the requested target ref. If the user did not specify one, use `origin/main`.
2. Keep one upgrade attempt inside one worker execution context:
   - Prefer one command task that runs the configured upgrade entrypoint from start to finish.
   - If using an AI CLI worker, assign one worker task and let that worker keep one interactive PTY session for the full attempt: update, tests, build, restart, health check, rollback decision, final report.
   - Do not split the same upgrade attempt across multiple workers, multiple AI agents, or several separate worker tasks; that loses local state and makes rollback reasoning weaker.
3. Run the upgrade entrypoint configured for that deployment. If no host-local wrapper exists, run the repository script with explicit environment variables:

   ```bash
   cd "$NBCO_REPO_DIR"
   NBCO_APP_DIR="$NBCO_APP_DIR" \
   NBCO_CONFIG="$NBCO_CONFIG" \
   NBCO_HEALTH_URL="$NBCO_HEALTH_URL" \
   scripts/upgrade-nbco.sh origin/main
   ```

4. Do not hand-write the deploy sequence unless the script itself is broken.
5. Treat a successful run only as one that ends with the service healthy at the deployment's configured health URL.
6. If the script reports rollback succeeded, tell the user the upgrade failed but production was restored to the previous binary.
7. If both upgrade and rollback health checks fail, report that production needs manual intervention and include the last `journalctl -u nbco` errors.

## Constraints

- Do not skip tests unless the superadmin explicitly asks for an emergency deploy.
- Do not deploy from a dirty production repo.
- Deployment-specific paths, domains, ports, and service names must come from environment variables or a host-local wrapper. Do not commit those facts into the generic script or skill.
- `scripts/upgrade-nbco.sh` is the repository source for the upgrade logic.
- Do not restart `nbco-worker` as part of an nbco upgrade unless the worker binary itself changed.
- Do not reuse Claude/Codex sessions across unrelated tasks. The continuity requirement applies only within one upgrade attempt.
- Do not expose tokens, config secrets, or environment dumps in the progress report.
- During `systemctl restart nbco`, Telegram/HTTP may be unavailable for a few seconds. This is expected. The worker service is separate and continues running.
