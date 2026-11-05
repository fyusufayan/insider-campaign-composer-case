<script setup lang="ts">
import type { FieldError, LengthStats } from '@/api/types'
import FieldMessages from './FieldMessages.vue'
import LengthCounter from './LengthCounter.vue'

defineProps<{
  modelValue: string
  disabled: boolean
  stats: LengthStats | null
  limit: number
  checking: boolean
  errors: FieldError[]
  warnings: FieldError[]
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
</script>

<template>
  <div class="field" data-testid="sms-editor">
    <label class="field-label" for="content-sms">SMS text</label>
    <textarea
      id="content-sms"
      class="textarea"
      rows="4"
      :value="modelValue"
      :disabled="disabled"
      :aria-invalid="errors.length > 0"
      aria-describedby="content-sms-help"
      spellcheck="true"
      data-testid="sms-input"
      @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)"
    />
    <LengthCounter :stats="stats" :limit="limit" :typed="modelValue" :checking="checking" testid="sms-counter" />
    <p id="content-sms-help" class="field-help">
      Hard limit {{ limit }} characters in the worst case: every placeholder counts with its maximum length.
    </p>
    <FieldMessages :errors="errors" :warnings="warnings" testid="sms-messages" />
  </div>
</template>
