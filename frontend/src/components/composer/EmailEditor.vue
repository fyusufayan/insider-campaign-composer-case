<script setup lang="ts">
import type { FieldError, LengthStats } from '@/api/types'
import FieldMessages from './FieldMessages.vue'
import LengthCounter from './LengthCounter.vue'

defineProps<{
  subject: string
  body: string
  disabled: boolean
  subjectStats: LengthStats | null
  bodyStats: LengthStats | null
  subjectLimit: number
  bodyLimit: number
  checking: boolean
  subjectErrors: FieldError[]
  subjectWarnings: FieldError[]
  bodyErrors: FieldError[]
  bodyWarnings: FieldError[]
}>()
const emit = defineEmits<{ 'update:subject': [value: string]; 'update:body': [value: string] }>()
</script>

<template>
  <div data-testid="email-editor">
    <div class="field">
      <label class="field-label" for="content-subject">Email subject</label>
      <input
        id="content-subject"
        class="input"
        type="text"
        :value="subject"
        :disabled="disabled"
        :aria-invalid="subjectErrors.length > 0"
        data-testid="subject-input"
        @input="emit('update:subject', ($event.target as HTMLInputElement).value)"
      />
      <LengthCounter
        :stats="subjectStats"
        :limit="subjectLimit"
        :typed="subject"
        :checking="checking"
        testid="subject-counter"
      />
      <FieldMessages :errors="subjectErrors" :warnings="subjectWarnings" testid="subject-messages" />
    </div>
    <div class="field">
      <label class="field-label" for="content-body">Email body</label>
      <textarea
        id="content-body"
        class="textarea"
        rows="9"
        :value="body"
        :disabled="disabled"
        :aria-invalid="bodyErrors.length > 0"
        aria-describedby="content-body-help"
        data-testid="body-input"
        @input="emit('update:body', ($event.target as HTMLTextAreaElement).value)"
      />
      <LengthCounter :stats="bodyStats" :limit="bodyLimit" :typed="body" :checking="checking" testid="body-counter" />
      <p id="content-body-help" class="field-help">
        Plain text only. The unsubscribe footer is added by the system and does not count towards the limit.
      </p>
      <FieldMessages :errors="bodyErrors" :warnings="bodyWarnings" testid="body-messages" />
    </div>
  </div>
</template>
