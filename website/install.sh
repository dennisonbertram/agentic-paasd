#!/usr/bin/env bash
set -e

REPO="https://raw.githubusercontent.com/dennisonbertram/agentic-hosting/main"
SKILL_DIR="$HOME/.claude/skills/agentic-hosting"
CMD_DIR="$HOME/.claude/commands"

echo "Installing agentic-hosting Claude Code skill..."

mkdir -p "$SKILL_DIR" "$CMD_DIR"

# Download skill
curl -fsSL "$REPO/.claude/skills/agentic-hosting/SKILL.md" -o "$SKILL_DIR/SKILL.md"

# Download commands
for cmd in deploy status db logs; do
  curl -fsSL "$REPO/.claude/commands/$cmd.md" -o "$CMD_DIR/$cmd.md"
done

echo ""
echo "✓ Skill installed to $SKILL_DIR"
echo "✓ Commands installed: /deploy /status /db /logs"
echo ""
echo "Next: set your server credentials in your shell profile:"
echo ""
echo "  export AH_URL=https://your-server"
echo "  export AH_KEY=your-keyid.secret"
echo ""
echo "Then open Claude Code and try: /status"
