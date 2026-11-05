<script setup lang="ts">
// Who produced the shown content version: the AI (model + prompt version,
// the demo generator called out as such) or a manual edit.
import { computed } from 'vue'
import type { ContentVersion } from '@/api/types'
import { DEMO_MODEL } from '@/api/types'
import { formatDateTime } from '@/composables/format'

const props = defineProps<{ version: ContentVersion }>()
const isDemo = computed(() => props.version.model === DEMO_MODEL)
</script>

<template>
  <span class="version-badge" data-testid="content-version-badge">
    <span class="badge badge-neutral" data-testid="content-version-number">Version {{ version.version }}</span>
    <template v-if="version.origin === 'ai'">
      <span
        v-if="isDemo"
        class="badge badge-warning"
        data-testid="content-origin"
        title="Written by the deterministic demo generator (fixed templates). No Claude model was called."
      >
        Demo generator · not a live model
      </span>
      <span v-else class="badge badge-primary" data-testid="content-origin">AI-generated · {{ version.model }}</span>
      <span v-if="version.prompt_version" class="badge badge-neutral">Prompt {{ version.prompt_version }}</span>
    </template>
    <span v-else class="badge badge-success" data-testid="content-origin">Edited manually</span>
    <span class="saved-at">Saved {{ formatDateTime(version.created_at) }}</span>
  </span>
</template>

<style scoped>
.version-badge {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.version-badge .badge {
  white-space: normal;
}
.saved-at {
  font-size: 12.5px;
  color: var(--c-muted);
}
</style>
