#!/usr/bin/env bash
# Shared path resolution for the project-isolated Hermes profile.

bb_hermes_repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"

if [ "${BB_ASSISTANT_DATA_HOME+x}" = x ]; then
  bb_hermes_data_candidate="$BB_ASSISTANT_DATA_HOME"
elif [ "${XDG_DATA_HOME+x}" = x ]; then
  bb_hermes_data_candidate="$XDG_DATA_HOME/bb-graph-assistant"
else
  bb_hermes_data_candidate="$HOME/.local/share/bb-graph-assistant"
fi

bb_hermes_data_root="$(python3 -c 'import os,sys; p=os.path.expanduser(sys.argv[1]); p=p if os.path.isabs(p) else os.path.join(sys.argv[2],p); print(os.path.realpath(p))' "$bb_hermes_data_candidate" "$bb_hermes_repo_root")"
bb_hermes_profile_home="${BB_ASSISTANT_HERMES_HOME:-$bb_hermes_data_root/hermes}"
bb_hermes_install_dir="${BB_ASSISTANT_HERMES_INSTALL_DIR:-$bb_hermes_data_root/hermes-agent}"
bb_hermes_bootstrap_home="$bb_hermes_data_root/hermes-bootstrap-home"

export HERMES_HOME="$bb_hermes_profile_home"
export HERMES_INSTALL_DIR="$bb_hermes_install_dir"
