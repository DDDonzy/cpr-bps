import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
export default defineConfig({
 plugins:[vue(),tailwindcss()],
 resolve:{dedupe:['vue']},
 build:{outDir:'../plugin/web',emptyOutDir:false,cssCodeSplit:false,lib:{entry:'app.ts',name:'BpsSettings',formats:['iife'],fileName:()=> 'app.js',cssFileName:'style'},minify:true},
 define:{'process.env.NODE_ENV':JSON.stringify('production')},
})
