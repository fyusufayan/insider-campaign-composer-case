<script setup lang="ts">
import { computed } from 'vue'
import type { ApiError } from '@/api/client'
import { presentError, type Operation } from '@/api/errors'
import FieldErrors from '@/components/composer/FieldErrors.vue'
import RawProposal from '@/components/composer/RawProposal.vue'

const props = withDefaults(
  defineProps<{
    error: ApiError
    op: Operation
    tone?: 'danger' | 'warning'
    retryLabel?: string
    busy?: boolean
  }>(),
  { tone: 'danger', retryLabel: 'Try again', busy: false },
)

const emit = defineEmits<{ retry: []; reload: []; revise: [] }>()

const p = computed(() => presentError(props.error, props.op))
</script>

<template>
  <div
    class="notice"
    :class="tone === 'danger' ? 'notice-danger' : 'notice-warning'"
    role="alert"
    data-testid="error-notice"
    :data-code="error.code"
  >
    <svg class="icon" viewBox="0 0 20 20" width="18" height="18" aria-hidden="true">
      <circle cx="10" cy="10" r="8.5" fill="none" stroke="currentColor" stroke-width="1.6" />
      <path d="M10 5.5v5.5M10 13.8v.4" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" />
    </svg>
    <div class="content">
      <p class="notice-title">{{ p.title }}</p>
      <p class="msg">{{ p.message }}</p>
      <p v-if="p.hint" class="hint">{{ p.hint }}</p>
      <FieldErrors :errors="error.details" />
      <RawProposal v-if="error.rawProposal" :text="error.rawProposal" />
      <div v-if="p.canRetry || p.canReload || p.canReviseBrief || $slots.actions" class="notice-actions">
        <button v-if="p.canRetry" type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="emit('retry')">
          {{ retryLabel }}
        </button>
        <button v-if="p.canReload" type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="emit('reload')">
          Reload latest campaign
        </button>
        <button v-if="p.canReviseBrief" type="button" class="btn btn-ghost btn-sm" @click="emit('revise')">
          Revise brief in a new campaign
        </button>
        <slot name="actions" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.icon {
  flex: none;
  margin-top: 2px;
}
.content {
  min-width: 0;
  flex: 1;
}
.msg {
  margin-top: 2px;
}
.hint {
  margin-top: 4px;
  font-size: 13.5px;
  opacity: 0.9;
}
</style>
