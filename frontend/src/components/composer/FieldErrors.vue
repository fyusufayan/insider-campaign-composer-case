<script setup lang="ts">
// Field-level problems from the error envelope ({path, code, message, allowed?}).
// All values are rendered as escaped text.
import type { FieldError } from '@/api/types'

defineProps<{ errors: FieldError[]; title?: string }>()
</script>

<template>
  <div v-if="errors.length" class="field-errors" data-testid="field-errors">
    <p v-if="title" class="fe-title">{{ title }}</p>
    <ul>
      <li v-for="(e, i) in errors" :key="i" data-testid="field-error">
        <code v-if="e.path" class="fe-path">{{ e.path }}</code>
        <span class="fe-msg">{{ e.message }}</span>
        <span class="fe-code">({{ e.code }})</span>
        <span v-if="e.allowed && e.allowed.length" class="fe-allowed">
          Allowed: <span v-for="(a, j) in e.allowed" :key="j" class="fe-chip">{{ a }}</span>
        </span>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.field-errors {
  margin-top: var(--space-3);
}
.fe-title {
  font-weight: 600;
  font-size: 13px;
  margin-bottom: 4px;
}
ul {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
li {
  font-size: 13.5px;
}
.fe-path {
  background: rgb(255 255 255 / 0.7);
  border: 1px solid currentColor;
  border-radius: 4px;
  padding: 0 4px;
  margin-right: 6px;
  opacity: 0.9;
}
.fe-code {
  margin-left: 6px;
  opacity: 0.75;
  font-size: 12px;
}
.fe-allowed {
  display: block;
  margin-top: 2px;
  font-size: 12.5px;
}
.fe-chip {
  display: inline-block;
  margin: 2px 4px 0 0;
  padding: 0 6px;
  border-radius: 4px;
  background: rgb(255 255 255 / 0.7);
  font-family: var(--font-mono);
  font-size: 12px;
}
</style>
