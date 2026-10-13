{{/*
Helpers shared by every template in the chart.

The naming convention is the standard Helm one:
  <release>-<chart>  for resources that are singletons per release
  <release>-<component> for resources that belong to a logical role
*/}}

{{- define "wallet.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "wallet.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "wallet.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "wallet.labels" -}}
app.kubernetes.io/name: {{ include "wallet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 }}
{{- end -}}

{{- define "wallet.selectorLabels" -}}
app.kubernetes.io/name: {{ include "wallet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Image reference. `tag` defaults to the chart appVersion but the Makefile always
passes an explicit immutable tag (the short git SHA) so that a redeploy of the
same source is a no-op for the kubelet.
*/}}
{{- define "wallet.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Dependency hostnames. These are the addresses the *pods* use to reach each
other, which is why they are cluster-DNS names and not the host-facing
localhost:port the developer's curl uses. A Service is created with the same
name by the matching dependency template, so the two must stay in step:
  wallet-<release>-postgresql, -keycloak, -localstack
*/}}
{{- define "wallet.postgresHost" -}}
{{- printf "%s-postgresql" (include "wallet.fullname" .) -}}
{{- end -}}

{{- define "wallet.keycloakHost" -}}
{{- printf "%s-keycloak" (include "wallet.fullname" .) -}}
{{- end -}}

{{- define "wallet.localstackHost" -}}
{{- printf "%s-localstack" (include "wallet.fullname" .) -}}
{{- end -}}

{{/*
Connection string for the wallet database. Built from the Service DNS name of
the in-cluster Postgres, or taken verbatim when postgresql.enabled=false (the
managed-database case).

sslmode=disable is correct for in-cluster traffic to the local Postgres and
would be replaced by verify-full once the dependency is a managed service with
TLS — which is the whole reason this is a helper and not a literal.
*/}}
{{- define "wallet.dbDsn" -}}
{{- if .Values.postgresql.enabled -}}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable"
      .Values.postgresql.auth.username
      .Values.postgresql.auth.password
      (include "wallet.postgresHost" .)
      .Values.postgresql.auth.database -}}
{{- else -}}
{{- .Values.externalDatabase.dsn -}}
{{- end -}}
{{- end -}}

{{/*
Init script run by the postgres image on an empty data directory.

Keycloak and the wallet share one server here and are separated by database
name. That is a laptop-shaped decision (one pod, one PVC) rather than a
production one, but it buys something compose does not: Keycloak gets a real
database instead of silently falling back to embedded H2, so its realm survives
a pod restart. The wallet's own schema is still owned by goose migrations at
app startup — this only creates the two databases.
*/}}
{{- define "wallet.postgresInitScript" -}}
#!/bin/sh
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	SELECT 'CREATE DATABASE keycloak OWNER $POSTGRES_USER'
	WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'keycloak')\gexec
EOSQL
{{- end -}}

{{/*
ServiceAccount used by the wallet pods. Created by serviceaccount.yaml only
when serviceAccount.create=true; otherwise the default SA is used, which is
what a cluster with no RBAC needs (the app talks to no Kubernetes API, so it
requires no permissions at all).
*/}}
{{- define "wallet.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "wallet.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
