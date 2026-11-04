<script setup lang="ts">
// Campaign creation form: name + natural-language brief. Client checks
// mirror the backend limits (UX only); server 422 details are mapped back to
// the fields by path and always shown.
import { computed, ref, watch } from 'vue'
import type { ApiError } from '@/api/client'
import { LIMITS } from '@/api/types'
import { runeCount, validateBrief, validateName } from '@/composables/validation'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'

const EXAMPLE_NAME = 'İstanbul cart recovery'
const EXAMPLE_BRIEF =
  'Send a short and friendly 30% discount reminder to users in Istanbul who added products to their cart in the last 30 days but did not complete a purchase.'

const props = withDefaults(
  defineProps<{
    pending?: boolean
    serverError?: ApiError | null
    initialName?: string
    initialBrief?: string
  }>(),
  { pending: false, serverError: null, initialName: '', initialBrief: '' },
)

const emit = defineEmits<{ submit: [value: { name: string; brief: string }] }>()

const name = ref(props.initialName)
const brief = ref(props.initialBrief)
const touched = ref({ name: false, brief: false })
const submitted = ref(false)
const nameEl = ref<HTMLInputElement | null>(null)
const briefEl = ref<HTMLTextAreaElement | null>(null)

const nameCount = computed(() => runeCount(name.value))
const briefCount = computed(() => runeCount(brief.value))
const nameClientError = computed(() => validateName(name.value))
const briefClientError = computed(() => validateBrief(brief.value))

// Server field errors, cleared for a field as soon as it is edited.
const serverFieldErrors = ref<Record<string, string>>({})
watch(
  () => props.serverError,
  (err) => {
    const map: Record<string, string> = {}
    if (err && err.code === 'validation_failed') {
      for (const d of err.details) {
        if ((d.path === 'name' || d.path === 'brief') && !map[d.path]) map[d.path] = d.message
      }
    }
    serverFieldErrors.value = map
  },
  { immediate: true },
)
watch(name, () => delete serverFieldErrors.value.name)
watch(brief, () => delete serverFieldErrors.value.brief)

/** Server errors not attributable to a form field (shown as a notice). */
const unmappedError = computed(() => {
  const err = props.serverError
  if (!err) return null
  if (err.code !== 'validation_failed') return err
  const rest = err.details.filter((d) => d.path !== 'name' && d.path !== 'brief')
  return rest.length || err.details.length === 0 ? err : null
})

const nameError = computed(
  () => serverFieldErrors.value.name ?? (touched.value.name || submitted.value ? nameClientError.value : null),
)
const briefError = computed(
  () => serverFieldErrors.value.brief ?? (touched.value.brief || submitted.value ? briefClientError.value : null),
)

function submit() {
  if (props.pending) return
  submitted.value = true
  if (nameClientError.value) {
    nameEl.value?.focus()
    return
  }
  if (briefClientError.value) {
    briefEl.value?.focus()
    return
  }
  emit('submit', { name: name.value, brief: brief.value })
}

function onBriefKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
    e.preventDefault()
    submit()
  }
}

function useExample() {
  if (!name.value.trim()) name.value = EXAMPLE_NAME
  brief.value = EXAMPLE_BRIEF
  briefEl.value?.focus()
}
</script>

<template>
  <form class="brief-form" novalidate data-testid="brief-form" @submit.prevent="submit">
    <div class="field">
      <div class="field-row">
        <label class="field-label" for="campaign-name">Campaign name</label>
        <span class="counter" :class="{ over: nameCount > LIMITS.nameMax }" aria-hidden="true">
          {{ nameCount }}/{{ LIMITS.nameMax }}
        </span>
      </div>
      <input
        id="campaign-name"
        ref="nameEl"
        v-model="name"
        class="input"
        type="text"
        name="name"
        autocomplete="off"
        placeholder="e.g. İstanbul cart recovery"
        :aria-invalid="nameError ? 'true' : 'false'"
        :aria-describedby="nameError ? 'campaign-name-error' : undefined"
        :disabled="pending"
        @blur="touched.name = true"
      />
      <p v-if="nameError" id="campaign-name-error" class="field-error" data-testid="name-error">{{ nameError }}</p>
    </div>

    <div class="field">
      <div class="field-row">
        <label class="field-label" for="campaign-brief">Campaign brief</label>
        <span class="counter" :class="{ over: briefCount > LIMITS.briefMax }" aria-hidden="true">
          {{ briefCount }}/{{ LIMITS.briefMax }}
        </span>
      </div>
      <p id="campaign-brief-help" class="field-help">
        Describe who to reach and what to say, in plain language. The AI proposes an audience rule; the backend
        validates it before anything is counted.
      </p>
      <textarea
        id="campaign-brief"
        ref="briefEl"
        v-model="brief"
        class="textarea"
        name="brief"
        rows="6"
        placeholder="e.g. Send a friendly 20% discount to users in Ankara who bought electronics in the last 60 days."
        :aria-invalid="briefError ? 'true' : 'false'"
        :aria-describedby="briefError ? 'campaign-brief-error campaign-brief-help' : 'campaign-brief-help'"
        :disabled="pending"
        @blur="touched.brief = true"
        @keydown="onBriefKeydown"
      />
      <p v-if="briefError" id="campaign-brief-error" class="field-error" data-testid="brief-error">
        {{ briefError }}
      </p>
    </div>

    <ErrorNotice v-if="unmappedError" class="form-error" :error="unmappedError" op="create" />

    <div class="actions">
      <button type="button" class="btn btn-ghost" :disabled="pending" @click="useExample">Use example brief</button>
      <button type="submit" class="btn btn-primary" :disabled="pending" data-testid="create-submit">
        <span v-if="pending" class="spinner" aria-hidden="true" />
        {{ pending ? 'Creating…' : 'Create campaign' }}
      </button>
    </div>
    <p class="kbd-hint">Tip: press <kbd>Ctrl</kbd>/<kbd>⌘</kbd> + <kbd>Enter</kbd> in the brief to submit.</p>
  </form>
</template>

<style scoped>
.form-error {
  margin-top: var(--space-5);
}
.actions {
  display: flex;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: var(--space-2);
  margin-top: var(--space-5);
}
.kbd-hint {
  margin-top: var(--space-3);
  text-align: right;
  font-size: 12px;
  color: var(--c-muted);
}
kbd {
  font-family: var(--font-mono);
  font-size: 11px;
  border: 1px solid var(--c-border-strong);
  border-bottom-width: 2px;
  border-radius: 4px;
  padding: 0 4px;
  background: var(--c-surface);
}
@media (max-width: 640px) {
  .actions .btn {
    flex: 1;
  }
}
</style>
