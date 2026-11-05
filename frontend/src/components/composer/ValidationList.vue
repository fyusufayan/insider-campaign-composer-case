<script setup lang="ts">
// Problems not tied to an editor field (path "" or "channels").
import type { FieldError } from '@/api/types'

defineProps<{ errors: FieldError[]; warnings: FieldError[] }>()
</script>

<template>
  <div v-if="errors.length || warnings.length" class="validation-list" data-testid="validation-list">
    <div v-if="errors.length" class="notice notice-danger" role="alert">
      <ul>
        <li v-for="(e, i) in errors" :key="i" data-testid="general-error">{{ e.message }} ({{ e.code }})</li>
      </ul>
    </div>
    <div v-if="warnings.length" class="notice notice-warning">
      <ul>
        <li v-for="(w, i) in warnings" :key="i" data-testid="general-warning">{{ w.message }} ({{ w.code }})</li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.validation-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
ul {
  margin: 0;
  padding-left: 18px;
  font-size: 13.5px;
}
</style>
