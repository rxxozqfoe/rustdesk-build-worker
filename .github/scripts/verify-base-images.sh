#!/usr/bin/env bash
# Verify sigstore/cosign signatures for every pinned base image in a Dockerfile.
#
# Flow:
#   1. Parse every `FROM ... @sha256:<64-hex>` reference out of the Dockerfile.
#   2. For each reference, strip tag/digest to get the bare image name.
#   3. Look up the name (prefix match) in .github/cosign-image-policy.json:
#       - policies[]  -> run `cosign verify` with the specified OIDC issuer
#                        and certificate-identity regex.
#       - unsigned[]  -> explicit skip, print the recorded reason.
#       - no match    -> FAIL. Any new base image must be classified in
#                        the policy file before it can land.
#
# This makes the workflow Dockerfile-agnostic: changing a FROM line or
# adding a new builder stage does not require touching the workflow.

set -euo pipefail

POLICY_FILE="${POLICY_FILE:-.github/cosign-image-policy.json}"
DOCKERFILE="${1:-Dockerfile}"

if [ ! -f "$POLICY_FILE" ]; then
  echo "::error::policy file not found: $POLICY_FILE"
  exit 1
fi
if [ ! -f "$DOCKERFILE" ]; then
  echo "::error::Dockerfile not found: $DOCKERFILE"
  exit 1
fi

# Extract any token on a FROM line that contains @sha256:<64-hex>.
# Handles `FROM --platform=$BUILDPLATFORM image:tag@sha256:... AS builder`.
mapfile -t refs < <(awk '
  /^[[:space:]]*FROM[[:space:]]/ {
    for (i = 1; i <= NF; i++) {
      if ($i ~ /@sha256:[a-f0-9]{64}$/) {
        print $i
      }
    }
  }
' "$DOCKERFILE")

if [ "${#refs[@]}" -eq 0 ]; then
  echo "::error::no digest-pinned FROM instructions found in $DOCKERFILE"
  exit 1
fi

echo "Discovered ${#refs[@]} pinned image reference(s):"
printf '  - %s\n' "${refs[@]}"
echo

failed=0

for ref in "${refs[@]}"; do
  # ref looks like: "image[:tag]@sha256:<digest>"
  name_tag="${ref%@*}"
  name="${name_tag%:*}"

  # Find a matching policy (signed) entry.
  policy_json=$(jq -c --arg n "$name" '
    [ .policies[]? | . as $p | select($n | startswith($p.match)) ] | first // empty
  ' "$POLICY_FILE")

  if [ -n "$policy_json" ]; then
    pname=$(   echo "$policy_json" | jq -r '.name')
    issuer=$(  echo "$policy_json" | jq -r '.issuer')
    identity=$(echo "$policy_json" | jq -r '.identity')

    echo "::group::Verify $ref (policy: $pname)"
    if cosign verify \
        --certificate-oidc-issuer="$issuer" \
        --certificate-identity-regexp="$identity" \
        "$ref" > /dev/null; then
      echo "OK signature verified against $identity"
    else
      echo "::error::cosign verify FAILED for $ref"
      failed=$((failed + 1))
    fi
    echo "::endgroup::"
    continue
  fi

  # Find a matching unsigned allow-list entry.
  unsigned_reason=$(jq -r --arg n "$name" '
    [ .unsigned[]? | . as $u | select($n | startswith($u.match)) ]
      | (first // {}).reason // empty
  ' "$POLICY_FILE")

  if [ -n "$unsigned_reason" ]; then
    echo "::warning::skipping $ref — explicitly listed as unsigned"
    echo "           reason: $unsigned_reason"
    continue
  fi

  # No entry at all — fail loud.
  echo "::error::no policy entry for image: $name"
  echo "::error::  Add it to \"policies\" (with cosign identity) or \"unsigned\""
  echo "::error::  (with a reason) in $POLICY_FILE before merging."
  failed=$((failed + 1))
done

if [ "$failed" -gt 0 ]; then
  echo "::error::$failed image(s) failed verification"
  exit 1
fi

echo
echo "All base images accounted for."
