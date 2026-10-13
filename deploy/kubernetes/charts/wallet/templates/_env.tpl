{{/*
Environment shared by every wallet process, whatever the role.

Called as:
  include "wallet.commonEnv" (dict "root" . "mode" "api" "dbMaxConns" 15
                                  "migrateOnStart" true "workersEnabled" false
                                  "serviceName" "wallet-api" "environment" "kind")

The role-specific keys (mode, pool size, worker gate, instance identity) are
parameters rather than values lookups, because they are what *distinguishes*
the three roles — passing them explicitly makes each call site self-documenting
and stops a copy-paste from silently giving `workers` the api's pool size.

The static, non-secret configuration comes from the ConfigMap via envFrom;
only what varies per role is rendered here.
*/}}
{{- define "wallet.commonEnv" -}}
{{- $root := .root -}}
- name: WALLET_MODE
  value: {{ .mode | quote }}
- name: WALLET_DB_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "wallet.fullname" $root }}-db
      key: WALLET_DB_DSN
- name: WALLET_DB_MAX_CONNS
  value: {{ .dbMaxConns | quote }}
- name: WALLET_MIGRATE_ON_START
  value: {{ .migrateOnStart | quote }}
- name: WALLET_WORKERS_ENABLED
  value: {{ .workersEnabled | quote }}
- name: WALLET_INSTANCE_ID
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: WALLET_ENV
  value: {{ .environment | quote }}
{{- if $root.Values.observability.otel.enabled }}
- name: WALLET_OTEL_ENDPOINT
  value: {{ $root.Values.observability.otel.endpoint | quote }}
- name: WALLET_OTEL_SERVICE_NAME
  value: {{ .serviceName | quote }}
{{- end }}
{{- if and $root.Values.sqs.localstack.enabled (not $root.Values.sqs.external.endpoint) }}
- name: WALLET_SQS_ENDPOINT
  value: "http://{{ include "wallet.localstackHost" $root }}:4566"
{{- end }}
- name: WALLET_SQS_REGION
  value: {{ $root.Values.sqs.external.region | quote }}
- name: WALLET_SQS_ACCESS_KEY_ID
  value: {{ $root.Values.sqs.external.accessKeyId | quote }}
- name: WALLET_SQS_SECRET_KEY
  value: {{ $root.Values.sqs.external.secretKey | quote }}
{{- range $k, $v := ($root.Values.app.extraEnv | default dict) }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}
