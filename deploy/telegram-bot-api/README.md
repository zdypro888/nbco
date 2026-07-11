# Official local Telegram Bot API

Use this deployment when nbco must receive Telegram files larger than the
cloud Bot API `getFile` limit. It keeps nbco on long polling; webhook delivery
is an independent choice and is not required.

## Runtime contract

- Build `tdlib/telegram-bot-api` from the official repository and install the
  binary at `/usr/local/bin/telegram-bot-api`.
- Create an unprivileged `telegram-bot-api` system user.
- Put `TELEGRAM_API_ID` and `TELEGRAM_API_HASH` in
  `/etc/nbco/telegram-bot-api.env` with mode `0600`. Never commit this file.
- Install `telegram-bot-api.service` in `/etc/systemd/system/` and the nbco
  drop-in as `/etc/systemd/system/nbco.service.d/local-api.conf`.
- Set nbco's `telegram_api_url` to `http://127.0.0.1:8081`.
- The Bot API port must differ from nbco's own `listen` port. If nbco uses
  `127.0.0.1:8081`, move this service to another loopback port and update
  `telegram_api_url` to match.

The service listens only on loopback. TLS termination is unnecessary because
nbco and the Bot API server communicate on the same host.

## Cloud-to-local migration

Telegram requires calling the cloud Bot API `logOut` method before the first
local login. Stop nbco first, call `logOut` exactly once, start the local
service, update `telegram_api_url`, then start nbco. Do not run the same bot on
cloud and local Bot API servers simultaneously.

Verify with `getMe`, `getWebhookInfo`, a normal message, and a file larger than
20 MiB. In local mode `getFile` may return an absolute path; nbco supports this
and copies the content into its own content-addressed file store.
