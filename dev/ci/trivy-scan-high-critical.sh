#!/usr/bin/env bash

cd "$(dirname "${BASH_SOURCE[0]}")"/../..
set -euo pipefail

OUTPUT=$(mktemp -d -t trivy_XXXX)
cleanup() {
  rm -rf "$OUTPUT"
}
trap cleanup EXIT

export GITHUB_TOKEN="${GH_TOKEN}"

# do not move this "set -x" above the GITHUB_TOKEN
# env var alias above, we don't want this to leak
# inside of CI's logs
set -x

# This is the special exit code that we tell trivy to use
# if finds a vulnerability

trivy_scan() {
  local templateFile="$1"
  local outputFile="$2"
  local target="$3"

  TRIVY_ARGS=(
    # fail the step if there is a vulnerability
    "--exit-code"
    "${VULNERABILITY_EXIT_CODE}"

    # ignore issues that we can't fix
    "--ignore-unfixed"

    # we'll only take action on higher CVEs
    "--severity"
    "HIGH,CRITICAL"

    # tell trivy to dump its output to an HTML file
    "--format"
    "template"

    # use the custom "trivy-html" template that we have in this folder
    "--template"
    "@${templateFile}"

    # dump the HTML output to a file named "outputFile"
    "--output"
    "${outputFile}"

    # scan the docker image named "target"
    "${target}"
  )

  trivy image "${TRIVY_ARGS[@]}"
}

upload_annotation() {
  local path="$1"
  local imageName="$2"

  local folder
  folder="$(dirname "${path}")"
  local file
  file="$(basename "${path}")"

  pushd "${folder}"

  buildkite-agent artifact upload "${file}"

  cat <<EOF | buildkite-agent annotate --style warning --context "Docker image security scan" --append
- **${imageName}** high/critical CVE(s): [${file}](artifact://${file})
EOF

  popd

  echo "High or critical severity CVEs were discovered in ${IMAGE}. Please read the buildkite annotation for more info."
}

ARTIFACT_FILE="${OUTPUT}/${IMAGE}-security-report.html"
trivy_scan "./dev/ci/trivy-artifact-html.tpl" "${ARTIFACT_FILE}" "${IMAGE}" || exitCode="$?"
case $exitCode in
0)
  # no vulnerabilities were found
  exit 0
  ;;
"${VULNERABILITY_EXIT_CODE}")
  # we found vulnerabilities - upload the annotation
  upload_annotation "${ARTIFACT_FILE}" "${IMAGE}"
  exit "${VULNERABILITY_EXIT_CODE}"
  ;;
*)
  # some other kind of error occurred
  exit $exitCode
  ;;
esac
