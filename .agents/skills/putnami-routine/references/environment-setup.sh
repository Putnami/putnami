curl -fsSL https://putnami.dev/install.sh | bash
export PATH="$HOME/.putnami/bin:$PATH"
for d in . */; do
  if [ -f "$d/putnami.workspace.json" ]; then (cd "$d" && putnami install); exit; fi
done
echo "no Putnami workspace found from $(pwd)" >&2
exit 1
