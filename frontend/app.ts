import { createApp } from 'vue'
import App from './App.vue'
import Logs from './Logs.vue'
import './style.css'

const syncTheme = () => document.documentElement.dataset.theme = window.codexProxyPlugin?.theme || 'light'
syncTheme()
window.addEventListener('codex-proxy-themechange', syncTheme)
createApp(document.getElementById('app')?.dataset.page === 'logs' ? Logs : App).mount('#app')
