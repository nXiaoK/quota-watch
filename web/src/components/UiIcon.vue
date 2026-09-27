<script setup lang="ts">
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 20 })
const paths: Record<string, string> = {
  pulse: 'M3 12h4l3-8 4 16 3-8h4',
  grid: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
  users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M16 3a4 4 0 0 1 0 8 M22 21v-2a4 4 0 0 0-3-3.87 M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0z',
  rules: 'M4 5h16 M4 12h16 M4 19h16 M8 3v4 M16 10v4 M10 17v4',
  history: 'M3 12a9 9 0 1 0 2.6-6.4 M3 3v5h5 M12 7v5l3 2',
  settings: 'M4 7h16 M4 17h16 M9 4v6 M15 14v6',
  plus: 'M12 5v14 M5 12h14',
  arrow: 'M5 12h14 M13 6l6 6-6 6',
  refresh: 'M20 7v-4l-3 1 M20 7a8 8 0 1 0 1 8 M20 7h-5',
  check: 'M5 12l4 4L19 6',
  close: 'M6 6l12 12 M6 18L18 6',
  link: 'M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-2 2 M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l2-2',
  bell: 'M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9 M10 21h4',
  shield: 'M12 3l8 3v6c0 5-8 9-8 9s-8-4-8-9V6z M8 12l3 3 5-6',
  search: 'M21 21l-5-5 M18 10a8 8 0 1 1-16 0 8 8 0 0 1 16 0z',
  chevron: 'M9 5l7 7-7 7',
  edit: 'M16 3l5 5 M3 21l5-1L21 7a3.5 3.5 0 0 0-5-5L3 16z',
  trash: 'M3 6h18 M9 6V3h6v3 M5 6l1 15h12l1-15 M10 10v7 M14 10v7',
  sun: 'M12 3V1 M12 23v-2 M3 12H1 M23 12h-2 M4.2 4.2l1.4 1.4 M18.4 18.4l1.4 1.4 M4.2 19.8l1.4-1.4 M18.4 5.6l1.4-1.4 M17 12a5 5 0 1 1-10 0 5 5 0 0 1 10 0z',
  moon: 'M21 13.1A9 9 0 0 1 10.9 3 9 9 0 1 0 21 13.1z',
  lock: 'M6 10h12v11H6z M8 10V6a4 4 0 0 1 8 0v4 M12 14v3',
  logout: 'M9 3H4v18h5 M14 7l5 5-5 5 M8 12h11',
}
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path :d="paths[name] ?? paths.pulse" />
  </svg>
</template>
