<script setup lang="ts">
import { computed } from 'vue'
import type { CampaignStatus } from '@/api/types'
import { STATUS_LABEL } from '@/composables/format'

const props = defineProps<{ status: CampaignStatus }>()
const cls = computed(() => {
  switch (props.status) {
    case 'draft':
      return 'badge-neutral'
    case 'segmented':
    case 'content_ready':
      return 'badge-primary'
    case 'approved':
    case 'completed':
      return 'badge-success'
    default:
      return 'badge-info'
  }
})
</script>

<template>
  <span class="badge" :class="cls" data-testid="status-badge">{{ STATUS_LABEL[status] ?? status }}</span>
</template>
