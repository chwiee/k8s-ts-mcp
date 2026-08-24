package k8sts.policy

import rego.v1

# input:
#   tier:           "readonly" | "nonprod-admin" | "prod-admin"  (resolved from AD group in Go)
#   cluster_env:    "prod" | "nonprod"
#   action_risk:    "safe" | "medium" | "high"                   (tagged by the playbook author)
#   execution_flag: bool                                          (the global dry-run/auto switch)
#
# result: {"allow": bool, "dry_run": bool, "reason": string}
#
# "allow" only means the caller may see/request this action. "dry_run" decides
# whether the command is actually run or just returned as text — it can be
# true even when allow is true, and it never depends solely on execution_flag.

default result := {"allow": false, "dry_run": true, "reason": "negado por padrão: nenhuma regra de política corresponde"}

# Alto risco: sempre exige aprovação humana, independente de tier ou da flag global.
result := {"allow": true, "dry_run": true, "reason": "ação de alto risco: sempre requer aprovação humana"} if {
	input.action_risk == "high"
}

# Tier readonly nunca executa, só diagnostica (para qualquer risco que não seja "high", já coberto acima).
result := {"allow": true, "dry_run": true, "reason": "tier readonly: apenas diagnóstico"} if {
	input.action_risk != "high"
	input.tier == "readonly"
}

# Risco seguro: qualquer tier acima de readonly pode executar, respeitando a flag global.
# ("not" só funciona como negação de literal no corpo da regra em Rego, não como
# valor dentro de um objeto — por isso a flag liga/desliga duas regras separadas
# em vez de uma expressão booleana inline.)
result := {"allow": true, "dry_run": false, "reason": "ação segura, dentro do tier do chamador"} if {
	input.action_risk == "safe"
	input.tier != "readonly"
	input.execution_flag == true
}

result := {"allow": true, "dry_run": true, "reason": "ação segura, dentro do tier do chamador, mas flag de execução está desligada"} if {
	input.action_risk == "safe"
	input.tier != "readonly"
	input.execution_flag == false
}

# Risco médio em cluster não-prod: qualquer tier admin pode executar.
result := {"allow": true, "dry_run": false, "reason": "ação de risco médio em não-prod, tier compatível"} if {
	input.action_risk == "medium"
	input.cluster_env == "nonprod"
	input.tier in {"nonprod-admin", "prod-admin"}
	input.execution_flag == true
}

result := {"allow": true, "dry_run": true, "reason": "ação de risco médio em não-prod, tier compatível, mas flag de execução está desligada"} if {
	input.action_risk == "medium"
	input.cluster_env == "nonprod"
	input.tier in {"nonprod-admin", "prod-admin"}
	input.execution_flag == false
}

# Risco médio em prod: só prod-admin pode executar.
result := {"allow": true, "dry_run": false, "reason": "ação de risco médio em prod, tier compatível"} if {
	input.action_risk == "medium"
	input.cluster_env == "prod"
	input.tier == "prod-admin"
	input.execution_flag == true
}

result := {"allow": true, "dry_run": true, "reason": "ação de risco médio em prod, tier compatível, mas flag de execução está desligada"} if {
	input.action_risk == "medium"
	input.cluster_env == "prod"
	input.tier == "prod-admin"
	input.execution_flag == false
}
