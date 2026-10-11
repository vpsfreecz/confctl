#!@BASH@ -p
# PREPARED ONLY: private relay mechanism under exact pinned Bash; no trap/eval/external command.
while IFS= builtin read -r record; do
  case "$record" in
    SigBlk:*|SigIgn:*|SigCgt:*|Threads:*) builtin printf '%s\n' "$record" >&4 || exit 72 ;;
  esac
done </proc/self/status
builtin printf 'READY\n' >&4 || exit 72
while IFS= builtin read -r request <&3; do
  case "$request" in
    TSTP) builtin kill -s TSTP 0 || exit 73 ;;
    TTIN) builtin kill -s TTIN 0 || exit 73 ;;
    TTOU) builtin kill -s TTOU 0 || exit 73 ;;
    *) exit 64 ;;
  esac
  builtin printf 'ACK %s\n' "$request" >&4 || exit 72
done
