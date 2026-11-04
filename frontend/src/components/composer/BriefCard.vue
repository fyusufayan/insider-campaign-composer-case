<script setup lang="ts">
// "What you asked": the saved brief exactly as written, plus the interpret
// action and its progress/error states.
import { computed } from 'vue'
import type { ApiError } from '@/api/client'
import type { Campaign } from '@/api/types'
import { useLlmProvider } from '@/composables/useLlmProvider'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'

const props = defineProps<{
  campaign: Campaign
  interpreting: boolean
  canInterpret: boolean
  busy: boolean
  error: ApiError | null
}>()

const emit = defineEmits<{ interpret: []; reload: []; revise: [] }>()

const provider = useLlmProvider()
const interpreted = computed(() => props.campaign.segment_rule !== null)
const actionLabel = computed(() => {
  if (props.interpreting) return 'Interpreting…'
  return interpreted.value ? 'Re-interpret brief' : 'Interpret brief'
})
const progressText = computed(() =>
  provider.value === 'demo'
    ? 'The demo interpreter is matching keywords in your brief and the backend is validating the proposed rule…'
    : 'The AI is turning your brief into a typed audience rule; the backend validates it before saving. This can take up to a minute.',
)
</script>

<template>
  <section class="card" aria-labelledby="asked-heading" data-testid="brief-card">
    <div class="card-header">
      <div>
        <p class="card-eyebrow"><span class="step-num">1</span> What you asked</p>
        <h2 id="asked-heading" class="card-title">Campaign brief</h2>
      </div>
    </div>
    <div class="card-body">
      <blockquote class="brief-text" data-testid="brief-text">{{ campaign.brief }}</blockquote>

      <div v-if="canInterpret" class="interpret-row">
        <button
          type="button"
          class="btn"
          :class="interpreted ? 'btn-secondary' : 'btn-primary'"
          :disabled="busy"
          :aria-busy="interpreting ? 'true' : 'false'"
          data-testid="interpret-button"
          @click="emit('interpret')"
        >
          <span v-if="interpreting" class="spinner" aria-hidden="true" />
          {{ actionLabel }}
        </button>
        <p v-if="interpreted && !interpreting" class="field-help">
          Re-interpreting replaces the targeting rule. Your channel selection is kept.
        </p>
      </div>
      <p v-else class="field-help locked">
        The brief can no longer be re-interpreted in this campaign status.
      </p>

      <p v-if="interpreting" class="progress" role="status" aria-live="polite" data-testid="interpret-progress">
        {{ progressText }}
      </p>

      <ErrorNotice
        v-if="error"
        class="error"
        :error="error"
        op="interpret"
        :busy="busy"
        retry-label="Retry interpretation"
        @retry="emit('interpret')"
        @reload="emit('reload')"
        @revise="emit('revise')"
      />
    </div>
  </section>
</template>

<style scoped>
.brief-text {
  margin: 0;
  padding: var(--space-3) var(--space-4);
  border-left: 3px solid var(--c-border-strong);
  background: var(--c-surface-2);
  border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  color: var(--c-text);
}
.interpret-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-3);
  margin-top: var(--space-4);
}
.locked {
  margin-top: var(--space-3);
}
.progress {
  margin-top: var(--space-3);
  font-size: 13.5px;
  color: var(--c-primary-text);
}
.error {
  margin-top: var(--space-4);
}
</style>
