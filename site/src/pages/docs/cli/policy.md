---
layout: ../../../layouts/DocsLayout.astro
title: mamori policy
---

# mamori policy

Emits a least-privilege access artifact derived entirely from the `source:` refs in the loaded packages. It resolves nothing, and never fabricates an identifier no ref carries: missing account IDs, projects, and store names show up as clearly-marked placeholders for you to fill in.

```bash
mamori policy [patterns...] [--type=Name] --format=aws-iam|gcp|external-secret
```

`--format` is required. A format whose relevant refs are empty still emits a valid, empty artifact plus a stderr note, never a misleadingly-complete success.

## --format=aws-iam

Grants `secretsmanager:GetSecretValue` on every `aws-sm://` ref and `ssm:GetParameter`/`ssm:GetParameters` on every `aws-ps://` ref. The account ID and region are not part of a ref (both come from ambient AWS config at resolve time), so every ARN uses a `*:*` placeholder for them.

```bash
$ mamori policy ./... --format=aws-iam
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SecretsManagerGetSecretValue",
      "Effect": "Allow",
      "Action": ["secretsmanager:GetSecretValue"],
      "Resource": ["arn:aws:secretsmanager:*:*:secret:prod/redis-pw"]
    }
  ]
}
```

## --format=gcp

Lists `roles/secretmanager.secretAccessor` and the `projects/<project>/secrets/<name>` resource name for every `gcp-sm://` ref. The project comes from the ref itself (`gcp-sm://<project>/<secret>`); `PROJECT` only appears as a placeholder for a malformed ref. This is a summary you turn into IAM bindings (one `gcloud secrets add-iam-policy-binding` per resource), not a ready-to-apply policy: a real binding also needs a principal, which no `source:` ref carries.

```bash
$ mamori policy ./... --format=gcp
{
  "role": "roles/secretmanager.secretAccessor",
  "resources": ["projects/my-project/secrets/redis-pw"]
}
```

## --format=external-secret

Emits an `external-secrets.io/v1` `ExternalSecret` manifest with one `spec.data` entry per `aws-sm://`, `aws-ps://`, or `gcp-sm://` ref. `spec.secretStoreRef.name` is always a placeholder (`REPLACE_ME_SECRET_STORE`): no `source:` ref names a Kubernetes `SecretStore`.

```yaml
$ mamori policy ./... --format=external-secret
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: mamori-managed-secrets
spec:
  secretStoreRef:
    name: REPLACE_ME_SECRET_STORE
    kind: SecretStore
  target:
    name: mamori-managed-secrets
  data:
    - secretKey: aws-sm-redis-pw
      remoteRef:
        key: prod/redis-pw
```

## Custom provider schemes

Sensitivity is computed from the same [built-in scheme set `mamori vet` uses](/docs/cli/vet/#what-it-flags), which only knows the providers mamori ships. If you [wrote your own provider](/docs/writing-a-provider/), name its scheme so its fields are treated as secrets here too:

```bash
mamori policy --format=aws-iam --secret-schemes=mysecrets ./...
```

The flag adds to the built-in set rather than replacing it, takes a bare scheme token (not a full ref), and is accepted by `explain`, `schema`, `policy`, `vet`, and `doctor --compare`, so every command agrees on what counts as a secret.

## See also

[`mamori explain`](/docs/cli/explain/) shows the refs this reads. [CLI overview](/docs/cli/).
