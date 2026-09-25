{{/*
Access master keys for the app container. With only
accessKeySecretName set, these render exactly the single-key configuration.
*/}}
{{- define "blaxsmith.accessKeyEnv" -}}
{{- $v := . -}}
{{- if and (not $v.accessKeySecretName) (or $v.accessKeyID $v.previousAccessKeys) -}}
{{- fail "accessKeyID and previousAccessKeys require accessKeySecretName" -}}
{{- end -}}
{{- $seen := dict ($v.accessKeyID | default "primary") true -}}
{{- if and $v.accessKeyID (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" $v.accessKeyID)) -}}
{{- fail "accessKeyID must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}" -}}
{{- end -}}
{{- $entries := list -}}
{{- range $v.previousAccessKeys -}}
{{- if or (not .id) (not .secretName) (not (regexMatch "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$" .id)) -}}
{{- fail "each previousAccessKeys entry needs a valid id and secretName" -}}
{{- end -}}
{{- if hasKey $seen .id -}}
{{- fail (printf "access key id %q is configured twice" .id) -}}
{{- end -}}
{{- $_ := set $seen .id true -}}
{{- $entries = append $entries (printf "%s=/run/blaxsmith/previous-access-keys/%s/key" .id .id) -}}
{{- end -}}
- name: BLAXSMITH_ACCESS_KEY_FILE
  value: /run/blaxsmith/access-key/key
{{- if $v.accessKeyID }}
- name: BLAXSMITH_ACCESS_KEY_ID
  value: {{ $v.accessKeyID | quote }}
{{- end }}
{{- if $entries }}
- name: BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES
  value: {{ join "," $entries | quote }}
{{- end }}
{{- end -}}

{{- define "blaxsmith.accessKeyMounts" -}}
- name: access-key
  mountPath: /run/blaxsmith/access-key
  readOnly: true
{{- range $i, $k := .previousAccessKeys }}
- name: previous-access-key-{{ $i }}
  mountPath: {{ printf "/run/blaxsmith/previous-access-keys/%s" $k.id }}
  readOnly: true
{{- end }}
{{- end -}}

{{- define "blaxsmith.accessKeyVolumes" -}}
- name: access-key
  secret:
    secretName: {{ .accessKeySecretName | quote }}
    items:
      - key: key
        path: key
    defaultMode: 288
{{- range $i, $k := .previousAccessKeys }}
- name: previous-access-key-{{ $i }}
  secret:
    secretName: {{ $k.secretName | quote }}
    items:
      - key: key
        path: key
    defaultMode: 288
{{- end }}
{{- end -}}
