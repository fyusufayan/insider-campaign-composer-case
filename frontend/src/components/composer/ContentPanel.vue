<script setup lang="ts">
// Step 4: message content. Generate (streamed), edit, validate (debounced,
// server-authoritative) and save immutable content versions for the
// selected channels. All state lives in the campaign store; this component
// only renders it and schedules the debounced validation.
import { computed, onBeforeUnmount, watch } from 'vue'
import { storeToRefs } from 'pinia'
import type { ContentField, FieldError } from '@/api/types'
import { useCampaignStore } from '@/stores/campaign'
import ErrorNotice from '@/components/ui/ErrorNotice.vue'
import ContentVersionBadge from './ContentVersionBadge.vue'
import EmailEditor from './EmailEditor.vue'
import EmailPreview from './EmailPreview.vue'
import GenerationProgress from './GenerationProgress.vue'
import SmsEditor from './SmsEditor.vue'
import SmsPreview from './SmsPreview.vue'
import ValidationList from './ValidationList.vue'

const props = withDefaults(defineProps<{ debounceMs?: number }>(), { debounceMs: 350 })
const emit = defineEmits<{ revise: [] }>()

const store = useCampaignStore()
const {
  campaign,
  content,
  contentLoading,
  contentError,
  draft,
  draftRevision,
  validatingDraft,
  draftValidationError,
  savingContent,
  saveError,
  saveNotice,
  generation,
  generating,
  busy,
  dirty,
  editableFields,
  canEditContent,
  canGenerate,
  shownValidation,
} = storeToRefs(store)

const LIMITS_FALLBACK = { sms_body: 160, email_subject: 80, email_body: 5000 }
const limits = computed(() => content.value?.limits ?? LIMITS_FALLBACK)
const example = computed(() => content.value?.example_values ?? null)
const savedVersion = computed(() => content.value?.content ?? null)
const hasSms = computed(() => editableFields.value.includes('sms_body'))
const hasEmail = computed(() => editableFields.value.includes('email_body'))
const disabled = computed(() => !canEditContent.value || generating.value || savingContent.value)

// ---- debounced validation (server-authoritative) ----
let timer: ReturnType<typeof setTimeout> | null = null
watch(draftRevision, () => {
  if (timer) clearTimeout(timer)
  timer = null
  if (!dirty.value) {
    void store.validateDraft() // aborts a pending validation; nothing to send
    return
  }
  timer = setTimeout(() => {
    timer = null
    void store.validateDraft()
  }, props.debounceMs)
})
// A kept draft must be re-validated for a new campaign version (reload after
// a conflict, channel change): validations are bound to the version.
watch(
  () => campaign.value?.version,
  () => {
    if (dirty.value && !timer) void store.validateDraft()
  },
)
onBeforeUnmount(() => {
  if (timer) clearTimeout(timer)
})

const checking = computed(() => dirty.value && (validatingDraft.value || shownValidation.value === null))

// ---- messages per field: current validation + server errors of a failed save ----
function forField(list: FieldError[] | undefined, f: ContentField): FieldError[] {
  return (list ?? []).filter((e) => e.path === f)
}
const saveDetails = computed(() => (saveError.value?.code === 'validation_failed' ? saveError.value.details : []))
function errorsOf(f: ContentField): FieldError[] {
  const fromSave = forField(saveDetails.value, f)
  return fromSave.length ? fromSave : forField(shownValidation.value?.errors, f)
}
function warningsOf(f: ContentField): FieldError[] {
  return forField(shownValidation.value?.warnings, f)
}
const FIELD_PATHS = ['sms_body', 'email_subject', 'email_body']
const generalErrors = computed(() => (shownValidation.value?.errors ?? []).filter((e) => !FIELD_PATHS.includes(e.path)))
const generalWarnings = computed(() =>
  (shownValidation.value?.warnings ?? []).filter((e) => !FIELD_PATHS.includes(e.path)),
)
const hasErrors = computed(() => (shownValidation.value?.errors.length ?? 0) > 0)
// Offer-policy findings: blocking for model output, advisory for marketer
// text. They are surfaced prominently so they are reviewed, not assumed safe.
const OFFER_CODES = ['unauthorized_offer', 'unauthorized_claim']
const offerFindings = computed(() =>
  (shownValidation.value?.warnings ?? []).filter((w) => OFFER_CODES.includes(w.code)),
)

// ---- previews (rendered by the server with fictional values) ----
const rendered = computed(() => shownValidation.value?.rendered_example ?? null)
const previewUnavailable = computed(() => {
  if (dirty.value && checking.value) return 'Updating the preview…'
  if (hasErrors.value) return 'Fix the errors above to see the preview.'
  if (!savedVersion.value && !dirty.value) return 'No content yet.'
  return 'Preview unavailable.'
})

const saveState = computed(() => {
  if (savingContent.value) return 'Saving…'
  if (dirty.value) return 'Unsaved changes'
  if (saveNotice.value === 'unchanged') return 'No changes — the text is identical to the saved version'
  if (savedVersion.value) return 'All changes saved'
  return 'Not saved yet'
})

const generateLabel = computed(() => (savedVersion.value ? 'Regenerate content' : 'Generate content'))
const generateHint = computed(() => {
  if (!campaign.value) return ''
  if (editableFields.value.length === 0) return 'Select at least one channel to write content.'
  if (!canEditContent.value) return 'Content cannot be changed in this status.'
  if (dirty.value) return 'Save or discard your edits before generating new content.'
  if (savedVersion.value) return 'Creates a new version; earlier versions are kept.'
  return 'Claude writes SMS/email copy for the selected channels from the content brief. No customer data is sent.'
})

const showGenerationError = computed(() => generation.value.phase === 'failed' && generation.value.error)
const conflict = computed(() => saveError.value?.code === 'version_conflict')
const saveErrorOther = computed(
  () => !!saveError.value && saveError.value.code !== 'validation_failed' && saveError.value.code !== 'version_conflict',
)
</script>

<template>
  <section class="card content-panel" data-testid="content-panel">
    <div class="card-header">
      <div class="head-main">
        <p class="card-eyebrow"><span class="step-num">4</span> Message content</p>
        <ContentVersionBadge v-if="savedVersion" :version="savedVersion" />
        <span v-else class="badge badge-neutral" data-testid="content-empty">No content yet</span>
      </div>
      <div class="head-actions">
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!canGenerate || busy"
          data-testid="generate-button"
          @click="store.generateContent()"
        >
          <span v-if="generating" class="spinner" aria-hidden="true" />
          {{ generating ? 'Generating…' : generateLabel }}
        </button>
      </div>
    </div>

    <div class="card-body body">
      <p class="hint" data-testid="generate-hint">{{ generateHint }}</p>

      <GenerationProgress v-if="generating" :view="generation" @cancel="store.cancelGeneration()" />
      <div
        v-else-if="generation.phase === 'done'"
        class="notice notice-info"
        role="status"
        data-testid="generation-done"
      >
        <div>
          <p class="notice-title">New content version generated and saved</p>
          <p>
            It passed the automatic checks (length limits, placeholders, keyword-based offer rules) and replaced the
            editor content. Review and edit it below.
          </p>
          <p v-for="(n, i) in generation.notes" :key="i" class="note" data-testid="generation-note">{{ n }}</p>
        </div>
        <button type="button" class="btn btn-ghost btn-sm" @click="store.dismissGeneration()">Dismiss</button>
      </div>
      <div
        v-else-if="generation.phase === 'cancelled'"
        class="notice notice-warning"
        role="status"
        data-testid="generation-cancelled"
      >
        <p>Generation cancelled. The latest saved content is shown.</p>
        <button type="button" class="btn btn-ghost btn-sm" @click="store.dismissGeneration()">Dismiss</button>
      </div>
      <ErrorNotice
        v-if="showGenerationError && generation.error"
        :error="generation.error"
        op="generate"
        retry-label="Generate again"
        :busy="busy || !canGenerate"
        @retry="store.generateContent()"
        @reload="store.reload()"
        @revise="emit('revise')"
      />

      <ErrorNotice v-if="contentError" :error="contentError" op="content" tone="warning" @retry="store.refreshContent()" />
      <p v-if="contentLoading && !content" class="loading" role="status">
        <span class="spinner" aria-hidden="true" /> Loading content…
      </p>

      <template v-if="editableFields.length">
        <p class="vars" data-testid="variables-help">
          Personalize with <code v-pre>{{first_name}}</code>, <code v-pre>{{last_name}}</code> or
          <code v-pre>{{city}}</code>; add a fallback after a bar, e.g. <code v-pre>{{first_name|there}}</code>.
        </p>
        <div class="channels" :class="{ two: hasSms && hasEmail }">
          <div v-if="hasSms" class="channel" data-testid="sms-section">
            <h3 class="channel-title">SMS</h3>
            <SmsEditor
              :model-value="draft.sms_body"
              :disabled="disabled"
              :stats="shownValidation?.sms ?? null"
              :limit="limits.sms_body"
              :checking="checking"
              :errors="errorsOf('sms_body')"
              :warnings="warningsOf('sms_body')"
              @update:model-value="store.setDraftField('sms_body', $event)"
            />
            <SmsPreview :text="rendered?.sms ?? null" :unavailable="previewUnavailable" />
          </div>
          <div v-if="hasEmail" class="channel" data-testid="email-section">
            <h3 class="channel-title">Email</h3>
            <EmailEditor
              :subject="draft.email_subject"
              :body="draft.email_body"
              :disabled="disabled"
              :subject-stats="shownValidation?.subject ?? null"
              :body-stats="shownValidation?.body ?? null"
              :subject-limit="limits.email_subject"
              :body-limit="limits.email_body"
              :checking="checking"
              :subject-errors="errorsOf('email_subject')"
              :subject-warnings="warningsOf('email_subject')"
              :body-errors="errorsOf('email_body')"
              :body-warnings="warningsOf('email_body')"
              @update:subject="store.setDraftField('email_subject', $event)"
              @update:body="store.setDraftField('email_body', $event)"
            />
            <EmailPreview :email="rendered?.email ?? null" :unavailable="previewUnavailable" />
          </div>
        </div>
        <p v-if="example" class="example" data-testid="example-values">
          Previews use fixed, fictional example values ({{ example.first_name }} {{ example.last_name }},
          {{ example.city }}), never customer data.
        </p>
      </template>
      <p v-else class="hint" data-testid="no-channels">No channel is selected, so there is no content to write.</p>

      <div v-if="offerFindings.length" class="notice notice-warning" role="alert" data-testid="offer-review">
        <div>
          <p class="notice-title">Review offers and claims before approval</p>
          <p>
            The text mentions {{ offerFindings.length === 1 ? 'an offer or price' : 'offers or prices' }} that the
            content brief does not authorize. Manually written text is not blocked, but nothing is approved by
            saving it: the approval step will check content, brief and offer again.
          </p>
        </div>
      </div>
      <p v-if="editableFields.length" class="disclaimer" data-testid="offer-disclaimer">
        Offer checks are keyword-based and advisory. No warning does not mean the copy is commercially or legally
        accurate; review every offer, price and claim yourself.
      </p>

      <ValidationList :errors="generalErrors" :warnings="generalWarnings" />
      <ErrorNotice
        v-if="draftValidationError"
        :error="draftValidationError"
        op="validateContent"
        tone="warning"
        @retry="store.validateDraft()"
      />

      <ErrorNotice
        v-if="conflict && saveError"
        :error="saveError"
        op="saveContent"
        :busy="busy"
        @reload="store.reload()"
      />
      <ErrorNotice v-else-if="saveErrorOther && saveError" :error="saveError" op="saveContent" @retry="store.saveContent()" />
      <div v-else-if="saveError?.code === 'validation_failed'" class="notice notice-danger" role="alert" data-testid="save-invalid">
        <p>Nothing was saved: fix the highlighted fields.</p>
      </div>

      <div v-if="editableFields.length" class="footer">
        <span class="save-state" :data-dirty="dirty" data-testid="save-state">{{ saveState }}</span>
        <div class="footer-actions">
          <button
            type="button"
            class="btn btn-ghost"
            :disabled="!dirty || savingContent || generating"
            data-testid="discard-button"
            @click="store.discardDraft()"
          >
            Discard changes
          </button>
          <button
            type="button"
            class="btn btn-primary"
            :disabled="!dirty || busy || !canEditContent || hasErrors || checking"
            data-testid="save-button"
            @click="store.saveContent()"
          >
            <span v-if="savingContent" class="spinner" aria-hidden="true" />
            {{ savingContent ? 'Saving…' : 'Save changes' }}
          </button>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.card-header {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--space-3);
}
.head-main {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-width: 0;
}
.body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.hint,
.loading {
  font-size: 13.5px;
  color: var(--c-muted);
}
.vars {
  font-size: 13px;
  color: var(--c-text-2);
}
.vars code {
  font-family: var(--font-mono);
  font-size: 12.5px;
  background: var(--c-surface-2);
  border: 1px solid var(--c-border);
  border-radius: 4px;
  padding: 0 4px;
}
.channels {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: var(--space-5);
}
.channels.two {
  grid-template-columns: minmax(0, 2fr) minmax(0, 3fr);
}
.channel {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  min-width: 0;
}
.channel-title {
  font-size: 15px;
  font-weight: 650;
}
.example,
.disclaimer {
  font-size: 12.5px;
  color: var(--c-muted);
}
.note {
  margin-top: 4px;
  font-size: 13px;
}
.notice {
  justify-content: space-between;
}
.footer {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  border-top: 1px solid var(--c-border);
  padding-top: var(--space-4);
}
.save-state {
  font-size: 13.5px;
  color: var(--c-success);
}
.save-state[data-dirty='true'] {
  color: var(--c-warning);
  font-weight: 600;
}
.footer-actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}
@media (max-width: 860px) {
  .channels.two {
    grid-template-columns: minmax(0, 1fr);
  }
  .head-actions,
  .head-actions .btn {
    width: 100%;
  }
}
</style>
