<script setup lang="ts">
// Progress of a streamed generation. The live text is the model's raw,
// unvalidated output for the current attempt: it is labelled as such, shown
// as escaped text, and never becomes the editor draft. Only the validated
// result (after the commit) replaces the content.
import { computed } from 'vue'
import type { ContentField } from '@/api/types'
import type { GenerationView } from '@/stores/campaign'

const props = defineProps<{ view: GenerationView }>()
const emit = defineEmits<{ cancel: [] }>()

const FIELD_LABEL: Record<ContentField, string> = {
  sms_body: 'SMS',
  email_subject: 'Email subject',
  email_body: 'Email body',
}

const STEPS = [
  { key: 'generating', label: 'Writing' },
  { key: 'validating', label: 'Validating' },
  { key: 'persisting', label: 'Saving' },
] as const

const stepIndex = computed(() => {
  switch (props.view.phase) {
    case 'connecting':
    case 'generating':
    case 'thinking':
    case 'repairing':
      return 0
    case 'validating':
      return 1
    case 'persisting':
      return 2
    default:
      return -1
  }
})

const headline = computed(() => {
  const v = props.view
  switch (v.phase) {
    case 'connecting':
      return 'Starting generation…'
    case 'thinking':
      return 'The model is thinking before it writes…'
    case 'generating':
      if (v.reason === 'repair') return 'Asking the model to fix the problems it made…'
      if (v.reason === 'truncation_retry') return 'The answer was cut off; retrying with a larger budget…'
      return 'The model is writing…'
    case 'validating':
      return 'Checking limits, placeholders and offer rules…'
    case 'repairing':
      return `The draft failed ${v.problems} check${v.problems === 1 ? '' : 's'}; one repair attempt follows.`
    case 'persisting':
      return 'Saving the validated content as a new version…'
    default:
      return ''
  }
})

const previewFields = computed(() => props.view.fields.filter((f) => props.view.preview[f] !== undefined))
</script>

<template>
  <section class="gen-progress" role="status" aria-live="polite" data-testid="generation-progress" :data-phase="view.phase">
    <div class="gen-head">
      <span class="spinner" aria-hidden="true" />
      <p class="headline" data-testid="generation-headline">{{ headline }}</p>
      <button type="button" class="btn btn-ghost btn-sm" data-testid="generation-cancel" @click="emit('cancel')">
        Cancel
      </button>
    </div>
    <ol class="steps">
      <li
        v-for="(s, i) in STEPS"
        :key="s.key"
        :class="{ active: i === stepIndex, done: i < stepIndex }"
        :data-testid="`generation-step-${s.key}`"
      >
        {{ s.label }}
      </li>
    </ol>
    <p v-if="view.attempt > 1" class="attempt" data-testid="generation-attempt">Model request {{ view.attempt }} of at most 3</p>
    <div v-if="previewFields.length" class="live" data-testid="generation-live">
      <p class="live-label">Live draft from the model — not validated, not saved</p>
      <div v-for="f in previewFields" :key="f" class="live-field" :data-testid="`live-${f}`">
        <span class="live-name">{{ FIELD_LABEL[f] }}</span>
        <pre>{{ view.preview[f] }}</pre>
      </div>
    </div>
  </section>
</template>

<style scoped>
.gen-progress {
  border: 1px solid var(--c-info-border);
  background: var(--c-info-soft);
  border-radius: var(--radius);
  padding: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.gen-head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}
.headline {
  flex: 1;
  font-weight: 600;
  color: var(--c-info);
  min-width: 0;
}
.steps {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
  margin: 0;
  padding: 0;
  list-style: none;
  font-size: 12.5px;
}
.steps li {
  padding: 2px 10px;
  border-radius: 999px;
  border: 1px solid var(--c-border-strong);
  color: var(--c-muted);
  background: var(--c-surface);
}
.steps li.active {
  border-color: var(--c-info);
  color: var(--c-info);
  font-weight: 600;
}
.steps li.done {
  border-color: var(--c-success);
  color: var(--c-success);
}
.attempt {
  font-size: 12.5px;
  color: var(--c-text-2);
}
.live {
  border-top: 1px dashed var(--c-info-border);
  padding-top: var(--space-3);
}
.live-label {
  font-size: 12px;
  font-weight: 600;
  color: var(--c-warning);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: 6px;
}
.live-field + .live-field {
  margin-top: var(--space-2);
}
.live-name {
  font-size: 12px;
  color: var(--c-muted);
}
.live pre {
  margin: 2px 0 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  font-family: var(--font-sans);
  font-size: 13.5px;
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--radius-sm);
  padding: 8px 10px;
  color: var(--c-text-2);
  max-height: 220px;
  overflow: auto;
}
</style>
