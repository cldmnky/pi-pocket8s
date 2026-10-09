# Pi agent configuration shipped in the image

These files are copied to `/usr/share/pi-agent/` in the image, and the
entrypoint seeds them into the workspace home on boot:

- `skills/agent-browser/SKILL.md` → `~/.pi/agent/skills/agent-browser/SKILL.md`
  (Pi loads it on matching tasks, or forced via `/skill:agent-browser …`).
- `prompts/agent-browser.md` → `~/.pi/agent/prompts/agent-browser.md`
  (becomes the `/agent-browser …` slash command in every session).

Seeding is copy-if-missing: edits inside the pod survive restarts, and
deleting a file re-seeds the shipped copy on the next pod start. That is also
how updates reach an existing volume — remove the old file (or directory) and
restart the pod.
