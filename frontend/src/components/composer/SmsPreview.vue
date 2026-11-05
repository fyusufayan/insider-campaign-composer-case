<script setup lang="ts">
// The rendered SMS for the fixed fictional example recipient. Escaped text
// in a message bubble (mustache interpolation only, never raw HTML).
defineProps<{ text: string | null; unavailable: string }>()
</script>

<template>
  <figure class="sms-preview" data-testid="sms-preview">
    <figcaption>SMS preview</figcaption>
    <div v-if="text !== null" class="bubble" data-testid="sms-preview-text">{{ text }}</div>
    <p v-else class="unavailable" data-testid="sms-preview-unavailable">{{ unavailable }}</p>
  </figure>
</template>

<style scoped>
.sms-preview {
  margin: 0;
}
figcaption {
  font-size: 12px;
  font-weight: 600;
  color: var(--c-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: 6px;
}
.bubble {
  background: #e9f2ff;
  border-radius: 16px 16px 16px 4px;
  padding: 10px 14px;
  max-width: 34ch;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-size: 14.5px;
  line-height: 1.45;
  color: var(--c-text);
}
.unavailable {
  font-size: 13px;
  color: var(--c-muted);
  font-style: italic;
}
</style>
