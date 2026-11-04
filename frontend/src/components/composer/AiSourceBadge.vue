<script setup lang="ts">
// Identifies what produced an interpretation. The demo interpreter is a
// deterministic keyword heuristic and is never presented as a live model.
import { computed } from 'vue'
import { DEMO_MODEL } from '@/api/types'

const props = defineProps<{ model: string; promptVersion?: string }>()
const isDemo = computed(() => props.model === DEMO_MODEL)
</script>

<template>
  <span class="ai-source" data-testid="ai-source">
    <span
      v-if="isDemo"
      class="badge badge-warning"
      title="Produced by the deterministic demo interpreter (keyword heuristic). No Claude model was called."
    >
      Demo interpreter · not a live model
    </span>
    <span v-else class="badge badge-primary">Model: {{ model }}</span>
    <span v-if="promptVersion" class="badge badge-neutral">Prompt {{ promptVersion }}</span>
  </span>
</template>

<style scoped>
.ai-source {
  display: inline-flex;
  flex-wrap: wrap;
  gap: 6px;
  min-width: 0;
  max-width: 100%;
}
.ai-source .badge {
  white-space: normal;
}
</style>
