#!/bin/sh
set -eu
umask 077
CONFIG="${1:?usage: pg-backup-config.sh <config.json> <directory> [keep-days]}"
DIR="${2:?missing backup directory}"
exec python3 "$(dirname "$0")/pg-backup-config.py" "$CONFIG" "$DIR" "${3:-14}"
