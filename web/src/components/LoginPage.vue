<script setup lang="ts">
import { ref } from 'vue'
import ThemeSwitch from './ThemeSwitch.vue'
import UiIcon from './UiIcon.vue'

const props = defineProps<{ checking: boolean; busy: boolean; error: string }>()
const emit = defineEmits<{ login: [credentials: { username: string; password: string }] }>()
const username = ref('')
const password = ref('')
const validation = ref('')

function submit() {
  if (props.busy || props.checking) return
  validation.value = ''
  if (!username.value || !password.value) {
    validation.value = '请填写用户名和密码。'
    return
  }
  emit('login', { username: username.value, password: password.value })
  password.value = ''
}
</script>

<template>
  <div class="auth-shell">
    <header class="auth-header">
      <div class="brand"><span class="brand-symbol"><UiIcon name="pulse" :size="25" /></span><span>Quota Watch<small>额度监控 · 订阅联动</small></span></div>
      <ThemeSwitch />
    </header>
    <main class="auth-main">
      <section class="auth-intro" aria-labelledby="auth-title">
        <div class="eyebrow">QUOTA AUTOMATION</div>
        <h1 id="auth-title">关注额度变化，<br />让联动有序发生。</h1>
        <p>在独立工作台中管理快照监控、重置通知与订阅联动。</p>
        <div class="auth-features">
          <div><span class="section-icon"><UiIcon name="pulse" :size="19" /></span><span><strong>已有快照监控</strong><small>读取主站保存的数据，不主动查询上游。</small></span></div>
          <div><span class="section-icon"><UiIcon name="bell" :size="19" /></span><span><strong>通知与联动规则</strong><small>按账号变化组织通知，按规则管理订阅。</small></span></div>
          <div><span class="section-icon"><UiIcon name="history" :size="19" /></span><span><strong>完整事件记录</strong><small>检查状态、执行结果与历史变化集中查看。</small></span></div>
        </div>
      </section>
      <section class="panel auth-card" aria-labelledby="login-title" :aria-busy="checking || busy">
        <span class="auth-card-icon"><UiIcon name="lock" :size="23" /></span>
        <h2 id="login-title">登录工作台</h2>
        <p class="auth-description">使用此监控服务的用户名和密码。</p>
        <div v-if="checking" class="auth-loading" role="status"><UiIcon name="refresh" :size="23" class="spinning" /><span>正在检查登录状态…</span></div>
        <form v-else class="auth-form" @submit.prevent="submit">
          <label class="field" for="login-username"><span>用户名</span><input id="login-username" v-model="username" name="username" type="text" autocomplete="username" autocapitalize="none" spellcheck="false" placeholder="输入用户名" required :disabled="busy" /></label>
          <label class="field" for="login-password"><span>密码</span><input id="login-password" v-model="password" name="password" type="password" autocomplete="current-password" placeholder="输入密码" required :disabled="busy" :aria-describedby="error || validation ? 'login-error' : undefined" /></label>
          <div v-if="error || validation" id="login-error" class="inline-message error auth-error" role="alert">{{ error || validation }}</div>
          <button class="button primary auth-submit" type="submit" :disabled="busy"><UiIcon :name="busy ? 'refresh' : 'arrow'" :size="18" :class="{ spinning: busy }" /> {{ busy ? '正在登录…' : '登录工作台' }}</button>
        </form>
        <p class="auth-note"><UiIcon name="shield" :size="14" /> 登录仅连接此监控服务。</p>
      </section>
    </main>
    <footer class="auth-footer"><span>Quota Watch</span><span>主站已有快照 · 独立运行</span></footer>
  </div>
</template>
