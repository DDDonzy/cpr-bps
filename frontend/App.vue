<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { BaseButton } from '@codex-proxy/ui/button'
import { BaseCard } from '@codex-proxy/ui/card'
import { BaseCheckbox } from '@codex-proxy/ui/checkbox'
import { BaseInput } from '@codex-proxy/ui/input'
import { BaseNumberInput } from '@codex-proxy/ui/number-input'
import { BaseScrollbar } from '@codex-proxy/ui/scrollbar'
import { BaseSelect } from '@codex-proxy/ui/select'
import { BaseSwitch } from '@codex-proxy/ui/switch'
import { BaseTag } from '@codex-proxy/ui/tag'
import { normalized, request, same, type Account, type Catalog, type Config, type Settings } from './api'

const settings=ref<Settings|null>(null)
const config=ref<Config>({enabled:false,accountIds:[],models:[],timeoutMs:3600000,onFailure:'native_fallback'})
const catalog=ref<Catalog>({accounts:[],models:[],warning:null})
const loading=ref(true), busy=ref(false), error=ref(''), status=ref(''), accountSearch=ref(''), modelSearch=ref('')
const locked=computed(()=>busy.value||!config.value.enabled)
const accounts=computed(()=>catalog.value.accounts.filter(a=>`${a.name} ${a.email||''} ${a.id}`.toLowerCase().includes(accountSearch.value.toLowerCase())))
const allModels=computed(()=>[...new Set([...catalog.value.models,...config.value.models])].sort())
const models=computed(()=>allModels.value.filter(m=>m.toLowerCase().includes(modelSearch.value.toLowerCase())))
const allModelsSelected=computed(()=>allModels.value.length>0&&allModels.value.every(m=>config.value.models.includes(m)))
const allSelected=computed(()=>catalog.value.accounts.length>0&&catalog.value.accounts.every(a=>config.value.accountIds.includes(a.id)))
const timeout=computed({get:()=>config.value.timeoutMs/60000,set:(value:number)=>{config.value.timeoutMs=value*60000;changed()}})
const policy=computed({get:()=>config.value.onFailure,set:(value:string)=>{config.value.onFailure=value as Config['onFailure'];changed()}})
const policyOptions=[{label:'回落原生',value:'native_fallback'},{label:'拒绝请求',value:'reject'}]
function changed(){status.value='有未保存的修改'}
function selectAccount(id:string,checked:boolean){const selected=new Set(config.value.accountIds);checked?selected.add(id):selected.delete(id);config.value.accountIds=[...selected];changed()}
function selectModel(id:string,checked:boolean){const selected=new Set(config.value.models);checked?selected.add(id):selected.delete(id);config.value.models=[...selected];changed()}
function toggleAllModels(){config.value.models=allModelsSelected.value?[]:[...allModels.value];changed()}
function toggleAll(){config.value.accountIds=allSelected.value?[]:catalog.value.accounts.map(a=>a.id);changed()}
// OAuth 身份与 CPR 账号管理页一致：邮箱用户名为主行，完整邮箱为次行。
function identity(a:Account){return a.email?.trim()||a.id}
function title(a:Account){return identity(a).split('@')[0]||identity(a)}
const tones=['bg-cp-blue-container-strong text-cp-blue-on-container','bg-cp-green-container-strong text-cp-green-on-container','bg-cp-orange-container-strong text-cp-orange-on-container','bg-cp-cyan-container-strong text-cp-cyan-on-container']
function avatarTone(a:Account){return tones[[...a.id].reduce((sum,c)=>sum+(c.codePointAt(0)||0),0)%tones.length]}
const plans:Record<string,string>={pro:'Pro',plus:'Plus',free:'Free',prolite:'Pro Lite'}
function plan(a:Account){return a.plan?plans[a.plan.toLowerCase()]||a.plan:''}
async function load(){
 busy.value=true;loading.value=true;error.value=''
 try{const [value,options]=await Promise.all([request<Settings>('GET','settings'),request<Catalog>('GET','catalog')]);settings.value=value;config.value=normalized(value.configuration);catalog.value=options;status.value='已保存';error.value=options.warning||''}
 catch(e){error.value=e instanceof Error?e.message:'无法读取设置'}finally{busy.value=false;loading.value=false}
}
async function save(){
 if(busy.value||!settings.value)return
 error.value='';const value=normalized(config.value)
 if(!Number.isInteger(value.timeoutMs)||value.timeoutMs<60000||value.timeoutMs>14400000){error.value='等待时间须为 1 至 240 分钟';return}
 busy.value=true
 try{
  try{await request('POST','settings',{revision:settings.value.revision,configuration:value})}
  catch(original){const actual=await request<Settings>('GET','settings');if(!same(actual.configuration,value))throw original}
  let current:Settings
  try{current=await request<Settings>('GET','settings')}
  catch{settings.value=null;status.value='已保存，正在刷新页面…';return}
  if(!same(current.configuration,value))throw new Error('配置已被更新，请重新加载核对')
  settings.value=current;config.value=normalized(current.configuration);status.value='已保存，立即生效'
 }catch(e){error.value=e instanceof Error?e.message:'保存失败'}finally{busy.value=false}
}
onMounted(load)
</script>

<template>
 <main class="mx-auto flex max-w-240 flex-col gap-4 p-4 text-cp-text">
  <div v-if="error" role="alert" class="rounded-cp bg-cp-error-container p-3 text-cp-error-on-container">{{error}}</div>
  <div v-if="loading" class="p-4 text-cp-text-secondary">正在读取设置…</div>
  <template v-else-if="settings||status.startsWith('已保存')">
   <BaseCard padding="compact">
    <div class="flex items-center justify-between gap-4">
     <div><h2 class="text-cp font-heavy">启用 BPS</h2><p class="mt-2 text-cp-sm text-cp-text-secondary">使用选定账号和模型，其余请求保持 CPR 原生处理</p></div>
     <BaseSwitch v-model="config.enabled" label="启用 BPS" :disabled="busy" @update:model-value="changed" />
    </div>
   </BaseCard>
   <div class="flex flex-col gap-4" :class="{'opacity-50':!config.enabled}" aria-label="BPS 路由设置">
    <BaseCard padding="compact">
     <div class="mb-3 flex items-center justify-between gap-3"><h2 class="text-cp font-heavy">使用账号</h2><div class="flex items-center gap-2"><span class="text-cp-sm text-cp-text-tertiary">已选 {{config.accountIds.length}} / {{catalog.accounts.length}} 个</span><BaseButton size="sm" :disabled="locked" @click="toggleAll">{{allSelected?'取消全选':'全选'}}</BaseButton></div></div>
     <BaseInput v-model="accountSearch" type="search" placeholder="搜索账号名称、邮箱或 ID" aria-label="搜索账号" :disabled="locked" class="mb-3" />
     <BaseScrollbar max-height="288px" aria-label="账号列表">
      <div class="flex flex-col gap-2 pr-2">
       <BaseCheckbox v-for="a in accounts" :key="a.id" :model-value="config.accountIds.includes(a.id)" :label="identity(a)" :disabled="locked" show-label class="w-full rounded-cp bg-cp-fill-quaternary p-3 [&>span:last-child]:flex-1" @update:model-value="value=>selectAccount(a.id,value)">
        <template #label><div class="flex min-w-0 items-center gap-3">
         <span class="inline-flex size-9 shrink-0 items-center justify-center rounded-lg text-cp font-extrabold" :class="avatarTone(a)">{{title(a).slice(0,1).toUpperCase()}}</span>
         <div class="min-w-0 flex-1"><div class="flex min-w-0 flex-wrap items-center gap-2"><span class="min-w-0 truncate text-cp font-heavy" :title="identity(a)">{{title(a)}}</span><BaseTag v-if="plan(a)" size="sm" type="primary" round>{{plan(a)}}</BaseTag><BaseTag size="sm" :type="a.enabled?'success':'neutral'" round>{{a.enabled?'已启用':'未启用'}}</BaseTag></div><div class="mt-1 truncate font-mono text-cp-xs text-cp-text-quaternary">{{identity(a)}}</div></div>
        </div></template>
       </BaseCheckbox>
       <p v-if="!accounts.length" class="p-3 text-cp-sm text-cp-text-tertiary">没有匹配的 OAuth 账号</p>
      </div>
     </BaseScrollbar>
    </BaseCard>
    <BaseCard padding="compact">
     <div class="mb-3 flex items-center justify-between gap-3"><h2 class="text-cp font-heavy">应用模型</h2><div class="flex items-center gap-2"><span class="text-cp-sm text-cp-text-tertiary">已选 {{config.models.length}} / {{allModels.length}} 个</span><BaseButton size="sm" :disabled="locked" :aria-label="allModelsSelected?'取消全选模型':'全选模型'" @click="toggleAllModels">{{allModelsSelected?'取消全选':'全选'}}</BaseButton></div></div>
     <BaseInput v-model="modelSearch" type="search" placeholder="搜索模型" aria-label="搜索模型" :disabled="locked" class="mb-3" />
     <BaseScrollbar max-height="272px" aria-label="模型列表"><div class="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-2 pr-2">
      <BaseCheckbox v-for="model in models" :key="model" :model-value="config.models.includes(model)" :label="model" :disabled="locked" show-label class="w-full rounded-cp bg-cp-fill-quaternary p-3" @update:model-value="value=>selectModel(model,value)" />
      <p v-if="!models.length" class="p-3 text-cp-sm text-cp-text-tertiary">没有匹配的模型</p>
     </div></BaseScrollbar>
    </BaseCard>
    <BaseCard padding="compact">
     <div class="flex items-center justify-between gap-4"><h2 class="text-cp font-heavy">失败处理</h2><BaseSelect v-model="policy" :options="policyOptions" :disabled="locked" aria-label="失败处理" class="w-44" /></div>
     <p class="mt-3 text-cp-sm text-cp-text-secondary">回落仅在确认可以安全重试时执行，拒绝模式直接返回错误</p>
     <div class="mt-5 flex items-center justify-between gap-4"><span class="text-cp font-emphasis">请求等待时间</span><BaseNumberInput v-model="timeout" label="请求等待时间" unit="min" :min="1" :max="240" :disabled="locked" class="w-44" /></div>
    </BaseCard>
   </div>
   <footer class="flex flex-wrap items-center justify-between gap-3 py-1"><span aria-live="polite" class="text-cp-sm text-cp-text-secondary">{{status}}</span><div class="flex gap-2"><BaseButton :disabled="busy" @click="load">重新加载</BaseButton><BaseButton variant="primary" :disabled="busy||!settings" :loading="busy" @click="save">保存设置</BaseButton></div></footer>
  </template>
 </main>
</template>
