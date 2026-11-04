<script setup lang="ts">
// The rejected model output of ai_output_invalid: untrusted text shown only
// in a collapsed diagnostic section, as escaped plain text (mustache, <pre>).
defineProps<{ text: string }>()
</script>

<template>
  <details class="diagnostic raw-proposal" data-testid="raw-proposal">
    <summary>Diagnostic: rejected AI output (untrusted, not saved)</summary>
    <pre>{{ text }}</pre>
  </details>
</template>

<style scoped>
.raw-proposal {
  margin-top: var(--space-3);
  color: var(--c-text);
}
</style>
