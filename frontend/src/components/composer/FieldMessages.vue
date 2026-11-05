<script setup lang="ts">
// Inline problems of one content field: errors (blocking) and warnings
// (advisory). Text is escaped (mustache only).
import type { FieldError } from '@/api/types'

defineProps<{ errors: FieldError[]; warnings: FieldError[]; testid: string }>()
</script>

<template>
  <ul v-if="errors.length || warnings.length" class="field-messages" :data-testid="testid">
    <li v-for="(e, i) in errors" :key="`e${i}`" class="msg-error" data-testid="inline-error" :data-code="e.code">
      {{ e.message }} <span class="code">({{ e.code }})</span>
    </li>
    <li v-for="(w, i) in warnings" :key="`w${i}`" class="msg-warning" data-testid="inline-warning" :data-code="w.code">
      {{ w.message }} <span class="code">({{ w.code }})</span>
    </li>
  </ul>
</template>

<style scoped>
.field-messages {
  margin: 6px 0 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 3px;
  font-size: 13px;
}
.msg-error {
  color: var(--c-danger);
}
.msg-error::before {
  content: '✕ ';
}
.msg-warning {
  color: var(--c-warning);
}
.msg-warning::before {
  content: '! ';
  font-weight: 700;
}
.code {
  opacity: 0.75;
  font-size: 12px;
}
</style>
