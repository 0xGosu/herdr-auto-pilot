.PHONY: hap-reinstall-from-github
# SHELL := /bin/bash is required for `set -o pipefail` in *-dev targets (tee log files).
# This affects ALL Make recipes in this file — all recipes must remain bash-compatible.
SHELL := /bin/bash


hap-reinstall-from-github:
	herdr plugin uninstall herd-auto-prompter || true
	herdr plugin install 0xGosu/herdr-auto-pilot --yes
	hap daemon --ensure