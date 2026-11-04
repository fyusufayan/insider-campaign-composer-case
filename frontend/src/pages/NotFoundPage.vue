<script setup lang="ts">
import { RouterLink } from 'vue-router'
</script>

<template>
  <div class="card nf" data-testid="not-found">
    <h1>Page not found</h1>
    <p>The page you are looking for does not exist.</p>
    <RouterLink :to="{ name: 'campaign-new' }" class="btn btn-primary">Create a campaign</RouterLink>
  </div>
</template>

<style scoped>
.nf {
  padding: var(--space-8);
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-3);
}
</style>
