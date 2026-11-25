{{- define "mcp-linux-over-ssh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "mcp-linux-over-ssh.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "mcp-linux-over-ssh.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "mcp-linux-over-ssh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "mcp-linux-over-ssh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mcp-linux-over-ssh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* "https" when the pod serves TLS itself, else "http". */}}
{{- define "mcp-linux-over-ssh.scheme" -}}
{{- if .Values.tls.enabled }}https{{ else }}http{{ end -}}
{{- end }}

{{- define "mcp-linux-over-ssh.servicePort" -}}
{{- .Values.service.port | default (ternary 443 80 .Values.tls.enabled) -}}
{{- end }}

{{- define "mcp-linux-over-ssh.serviceHost" -}}
{{- printf "%s.%s.svc" (include "mcp-linux-over-ssh.fullname" .) .Release.Namespace -}}
{{- end }}

{{/* In-cluster URL of /mcp, as a kagent RemoteMCPServer reaches it. */}}
{{- define "mcp-linux-over-ssh.serviceURL" -}}
{{- $port := include "mcp-linux-over-ssh.servicePort" . | int -}}
{{- $default := ternary 443 80 .Values.tls.enabled -}}
{{- printf "%s://%s.cluster.local" (include "mcp-linux-over-ssh.scheme" .) (include "mcp-linux-over-ssh.serviceHost" .) -}}
{{- if ne $port $default }}:{{ $port }}{{ end }}/mcp
{{- end }}

{{/* The actor JWT audience: set explicitly, or the Service's cluster-local URL. */}}
{{- define "mcp-linux-over-ssh.actorAudience" -}}
{{- .Values.actorJwt.audience | default (printf "%s://%s.cluster.local" (include "mcp-linux-over-ssh.scheme" .) (include "mcp-linux-over-ssh.serviceHost" .)) -}}
{{- end }}

{{/* Gateway listener certificate Secret. */}}
{{- define "mcp-linux-over-ssh.gatewayTLSSecretName" -}}
{{- default (printf "%s-tls" (include "mcp-linux-over-ssh.fullname" .)) .Values.certManager.secretName -}}
{{- end }}

{{/* The pod's serving certificate Secret. */}}
{{- define "mcp-linux-over-ssh.servingTLSSecretName" -}}
{{- .Values.tls.existingSecret | default (printf "%s-serving-tls" (include "mcp-linux-over-ssh.fullname" .)) -}}
{{- end }}

{{/* The ConfigMap holding the skills written in values (skills.inject). */}}
{{- define "mcp-linux-over-ssh.skillsConfigMapName" -}}
{{- printf "%s-skills" (include "mcp-linux-over-ssh.fullname" .) -}}
{{- end }}

{{/* Mount points inside the container. */}}
{{- define "mcp-linux-over-ssh.paths" -}}
config: /etc/mcp-linux-over-ssh/config.yaml
tls: /etc/mcp-linux-over-ssh/tls
issuerCA: /etc/mcp-linux-over-ssh/issuer-ca/ca.pem
kagentCA: /etc/mcp-linux-over-ssh/kagent-ca/ca.pem
skillsInject: /etc/mcp-linux-over-ssh/skills/inject
skillsConfigMaps: /etc/mcp-linux-over-ssh/skills/cm
sshDir: /home/mcp/.ssh
{{- end }}

{{/*
One skills.inject entry as the SKILL.md it becomes. `name` always comes from the
entry. A string frontmatter is the description, written as a JSON string so no
character in it can change the YAML; a map is written as given.
*/}}
{{- define "mcp-linux-over-ssh.skillMd" -}}
---
name: {{ .name }}
{{- if kindIs "map" .frontmatter }}
{{ toYaml (omit .frontmatter "name") }}
{{- else }}
description: {{ .frontmatter | toString | toJson }}
{{- end }}
---

{{ .skill | toString | trimSuffix "\n" }}
{{ end }}

{{/* The directory each skills.configMaps entry is mounted in. */}}
{{- define "mcp-linux-over-ssh.configMapSkillName" -}}
{{- .skill | default .name -}}
{{- end }}

{{/*
Value checks that span several templates. Included from deployment.yaml,
which always renders, so a bad combination fails every install/template/lint.
*/}}
{{- define "mcp-linux-over-ssh.validate" -}}
{{- if not .Values.ssh.command }}
{{- fail "ssh.command is required: the command line that opens one session (see the example in values.yaml)" }}
{{- end }}
{{- if not .Values.ssh.prompt_regex }}
{{- fail "ssh.prompt_regex is required: a regular expression matching the remote shell's prompt" }}
{{- end }}
{{- if or (not .Values.ssh.idle_timeout) (has (toString .Values.ssh.idle_timeout) (list "0" "0s" "0m" "0h")) }}
{{- fail "ssh.idle_timeout must be set and greater than zero (e.g. 30m): over HTTP nothing else ends a session whose agent has gone away" }}
{{- end }}
{{- if and .Values.gateway.enabled (not .Values.host) }}
{{- fail "host must be set when gateway.enabled is true" }}
{{- end }}
{{- if and (not .Values.actorJwt.ca.clusterTrustBundle.enabled) (not .Values.actorJwt.ca.configMap.name) }}
{{- fail "actorJwt needs the issuer's CA: enable actorJwt.ca.clusterTrustBundle or set actorJwt.ca.configMap.name" }}
{{- end }}
{{- if not .Values.actorJwt.kagentController.address }}
{{- fail "actorJwt.kagentController.address is required" }}
{{- end }}
{{- range .Values.actorJwt.allowedAgents }}
{{- $parts := splitList "/" . }}
{{- if or (ne (len $parts) 2) (not (index $parts 0)) (not (index $parts 1)) }}
{{- fail (printf "actorJwt.allowedAgents entry %q must be namespace/name" .) }}
{{- end }}
{{- end }}
{{- $skillNameRE := "^[a-z0-9]+(-[a-z0-9]+)*$" }}
{{- $seen := dict }}
{{- range $i, $s := .Values.skills.inject }}
{{- $name := $s.name | default "" | toString }}
{{- if not $name }}
{{- fail (printf "skills.inject[%d] has no name" $i) }}
{{- end }}
{{- if or (not (regexMatch $skillNameRE $name)) (gt (len $name) 64) }}
{{- fail (printf "skills.inject[%d]: name %q must be lowercase letters, digits and single hyphens, at most 64 characters" $i $name) }}
{{- end }}
{{- if hasKey $seen $name }}
{{- fail (printf "skills.inject[%d]: skill %q is defined more than once" $i $name) }}
{{- end }}
{{- $_ := set $seen $name true }}
{{- if not $s.frontmatter }}
{{- fail (printf "skills.inject[%d] (%s): frontmatter is required: the skill's description, saying when to use it" $i $name) }}
{{- end }}
{{- if kindIs "map" $s.frontmatter }}
{{- if not $s.frontmatter.description }}
{{- fail (printf "skills.inject[%d] (%s): a frontmatter map must have a description" $i $name) }}
{{- end }}
{{- else if not (kindIs "string" $s.frontmatter) }}
{{- fail (printf "skills.inject[%d] (%s): frontmatter must be a string (the description) or a map" $i $name) }}
{{- end }}
{{- if not ($s.skill | default "" | toString | trim) }}
{{- fail (printf "skills.inject[%d] (%s): skill is required: the body of the SKILL.md" $i $name) }}
{{- end }}
{{- end }}
{{- range $i, $c := .Values.skills.configMaps }}
{{- if not $c.name }}
{{- fail (printf "skills.configMaps[%d] has no name (the ConfigMap's name)" $i) }}
{{- end }}
{{- $name := include "mcp-linux-over-ssh.configMapSkillName" $c }}
{{- if or (not (regexMatch $skillNameRE $name)) (gt (len $name) 64) }}
{{- fail (printf "skills.configMaps[%d]: skill name %q must be lowercase letters, digits and single hyphens, at most 64 characters; set `skill` when the ConfigMap's name is not the skill's name" $i $name) }}
{{- end }}
{{- if hasKey $seen $name }}
{{- fail (printf "skills.configMaps[%d]: skill %q is defined more than once" $i $name) }}
{{- end }}
{{- $_ := set $seen $name true }}
{{- end }}
{{- if .Values.tls.enabled }}
{{- $sources := 0 }}
{{- if .Values.tls.certManager.enabled }}{{ $sources = add1 $sources }}{{ end }}
{{- if .Values.tls.existingSecret }}{{ $sources = add1 $sources }}{{ end }}
{{- if .Values.tls.volume }}{{ $sources = add1 $sources }}{{ end }}
{{- if gt (int $sources) 1 }}
{{- fail "tls: set exactly one of tls.certManager.enabled, tls.existingSecret and tls.volume" }}
{{- end }}
{{- if eq (int $sources) 0 }}
{{- fail "tls.enabled needs a certificate: tls.certManager.enabled with an issuerRef, tls.existingSecret, or tls.volume" }}
{{- end }}
{{- if and .Values.tls.certManager.enabled (not .Values.tls.certManager.issuerRef.name) }}
{{- fail "tls.certManager.issuerRef.name is required (an Issuer that can sign the Service's *.svc names)" }}
{{- end }}
{{- if or (not .Values.tls.certFile) (not .Values.tls.keyFile) }}
{{- fail "tls.certFile and tls.keyFile are both required" }}
{{- end }}
{{- if and .Values.gateway.enabled (not .Values.gateway.backendTLS.enabled) }}
{{- fail "gateway.enabled with tls.enabled: the Gateway must speak TLS to the pod. Enable gateway.backendTLS (with caCertificateRefs or wellKnownCACertificates), or turn tls off" }}
{{- end }}
{{- end }}
{{- if and .Values.gateway.enabled .Values.gateway.backendTLS.enabled }}
{{- if not .Values.tls.enabled }}
{{- fail "gateway.backendTLS needs tls.enabled: the pod serves plain HTTP otherwise" }}
{{- end }}
{{- if and (not .Values.gateway.backendTLS.caCertificateRefs) (not .Values.gateway.backendTLS.wellKnownCACertificates) }}
{{- fail "gateway.backendTLS needs caCertificateRefs or wellKnownCACertificates" }}
{{- end }}
{{- end }}
{{- if .Values.gateway.enabled }}
{{- if not .Values.gateway.parentRefs }}
{{- fail "gateway.enabled requires at least one entry in gateway.parentRefs" }}
{{- end }}
{{- if .Values.gateway.httpRedirect.enabled }}
{{- if not .Values.gateway.httpRedirect.sectionName }}
{{- fail "gateway.httpRedirect.enabled requires gateway.httpRedirect.sectionName (the plain-HTTP listener)" }}
{{- end }}
{{- range .Values.gateway.parentRefs }}
{{- if not .sectionName }}
{{- fail (printf "gateway.httpRedirect.enabled requires a sectionName on every gateway.parentRefs entry (%q has none); otherwise the main route and the redirect both attach to the HTTP listener" (.name | default "")) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
