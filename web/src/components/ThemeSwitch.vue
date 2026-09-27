<script setup lang="ts">
import { setTheme, theme } from '../theme'
import UiIcon from './UiIcon.vue'
</script>

<template>
  <div class="theme-switch" role="group" aria-label="外观模式">
    <button type="button" aria-label="白天" :aria-pressed="theme === 'light'" :class="{ active: theme === 'light' }" @click="setTheme('light')"><UiIcon name="sun" :size="15" /><span>白天</span></button>
    <button type="button" aria-label="黑夜" :aria-pressed="theme === 'dark'" :class="{ active: theme === 'dark' }" @click="setTheme('dark')"><UiIcon name="moon" :size="15" /><span>黑夜</span></button>
  </div>
</template>
