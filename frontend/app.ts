import { createApp } from 'vue'
import App from './App.vue'
import './style.css'

const syncTheme = () => document.documentElement.dataset.theme = window.codexProxyPlugin?.theme || 'light'
syncTheme()
window.addEventListener('codex-proxy-themechange', syncTheme)
createApp(App).mount('#app')
