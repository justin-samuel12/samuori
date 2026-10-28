---
title: Validation results
section: Building images
summary: The checks Samuori runs on every image, and what to do about findings.
roles: [developer, maintainer, publisher, security-reviewer]
order: 50
---
Every built image is validated before it can be published. The **Validation** section of a build shows the verdict
and each check.

## Checks

- **Application check:** the check you chose in **Checks**, for example that the image starts and answers HTTP.
- **Sensitive content:** keys, credentials and secret files in the final image, found by path rules and content
  detectors. Findings show the path, layer and rule, never the content.
- **SBOM:** the list of software in the image.
- **Policies** your organization set.

## Verdicts

- **Passed:** the image may be published, subject to approvals.
- **Failed:** fix the cause and build again.
- **Blocked:** a policy refuses the image, for example sensitive content without an approved exception.
- **Incomplete:** a mandatory check could not finish. It blocks publication; it never passes silently.

## Images that must contain sensitive files

Some images, such as database images with installed certificates, are sensitive on purpose. A maintainer requests
an **output exception** on the project page; an administrator approves it for specific paths and up to 90 days.
The image is then marked **restricted** and can be published only to destinations that accept restricted images.
Private key files and the exact bytes of a secret given to the build are always blocked.
