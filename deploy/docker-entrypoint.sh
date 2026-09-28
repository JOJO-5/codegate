#!/bin/sh
set -eu

secret_source=${CODEGATE_JWT_SECRET_FILE:?CODEGATE_JWT_SECRET_FILE is required}
secret_target=/run/codegate-jwt-secret
cp "$secret_source" "$secret_target"
chown codegate:codegate "$secret_target"
chmod 0400 "$secret_target"
export CODEGATE_JWT_SECRET_FILE="$secret_target"

exec su-exec codegate "$@"
