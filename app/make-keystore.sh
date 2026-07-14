#!/usr/bin/env bash
# One-time: generate a signing key for the Holiday Map Android app and print the
# four GitHub secrets the build workflow needs. Keep holiday-map.keystore SAFE
# and OFF git — losing it means future updates can't install over the old app
# (Android rejects a changed signature). Back it up (it's in your Vaultwarden-worthy pile).
set -euo pipefail

KS="holiday-map.keystore"
ALIAS="holidaymap"

if [ -f "$KS" ]; then
  echo "$KS already exists — refusing to overwrite. Delete it first if you really mean to." >&2
  exit 1
fi

read -rsp "Choose a keystore password: " KSPASS; echo
read -rsp "Confirm password: " KSPASS2; echo
[ "$KSPASS" = "$KSPASS2" ] || { echo "Passwords don't match." >&2; exit 1; }

keytool -genkeypair -v \
  -keystore "$KS" \
  -alias "$ALIAS" \
  -keyalg RSA -keysize 2048 -validity 10000 \
  -storepass "$KSPASS" -keypass "$KSPASS" \
  -dname "CN=Holiday Map, OU=Homelab, O=Homelab, L=, ST=, C=GB"

echo
echo "=================================================================="
echo "Add these four repository secrets (Settings → Secrets and variables"
echo "→ Actions → New repository secret):"
echo "=================================================================="
echo "ANDROID_KEYSTORE_BASE64  = (the long string below)"
echo "ANDROID_KEYSTORE_PASSWORD= <the password you just chose>"
echo "ANDROID_KEY_ALIAS        = $ALIAS"
echo "ANDROID_KEY_PASSWORD     = <the same password>"
echo "------------------------------------------------------------------"
echo "ANDROID_KEYSTORE_BASE64 value:"
base64 -w0 "$KS"; echo
echo "------------------------------------------------------------------"
echo "Done. Back up $KS somewhere safe and do NOT commit it."
