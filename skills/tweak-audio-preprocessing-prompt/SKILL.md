---
name: tweak-audio-preprocessing-prompt
description: "Propose a concise update to an audio-preprocessing prompt when text is spoken incorrectly by TTS. Use when the user provides a concrete mispronunciation example."
---

# Tweak Audio Preprocessing Prompt

Use the user's example of source text and its incorrect spoken rendering to propose a focused revision to the audio-preprocessing prompt.

First, locate the active/default prompt if it is available. State the proposed prompt in full (or a minimal replacement if the prompt is not available) and briefly explain how it addresses the example.

Keep the resulting prompt below 200 tokens. Prefer broad, generally useful principles over a catalog of literal replacements. Give each principle no more than one or two representative examples. Preserve the prompt's existing purpose and constraints; do not change application behavior or apply the revision unless the user asks.
