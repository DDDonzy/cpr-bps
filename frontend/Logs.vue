<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { BaseButton } from '@codex-proxy/ui/button'
import { BaseCard } from '@codex-proxy/ui/card'
import { BaseInput } from '@codex-proxy/ui/input'
import { BaseModal } from '@codex-proxy/ui/modal'
import { BaseSegmented } from '@codex-proxy/ui/segmented'
import { BaseSelect } from '@codex-proxy/ui/select'
import { BaseTable, BaseTablePagination, type BaseTableColumn } from '@codex-proxy/ui/table'
import { BaseTag } from '@codex-proxy/ui/tag'
import { request } from './api'

type Fact={channel:string;reason?:string;relatedRequestId?:string|null}
type RecordRow={id:string;requestId?:string|null;createdAt?:string;createdAtDisplay?:string;accountEmail?:string|null;accountName?:string|null;clientApiKeyName?:string|null;model?:string|null;requestedModel?:string|null;route:string;clientTransport?:string;originalClientTransport?:string;originalRoute?:string;upstreamTransport?:string;bpsRoute?:Fact;message?:string;providerErrorCode?:string;failureClass?:string;logicalOutcome?:string;attemptCount?:number;latencyMs?:number|null;reasoningEffort?:string|null}
type List={items:RecordRow[];currentPage:number;pageSize:number;total:number;bpsWarning?:string}
const kind=ref('usage'),range=ref('24'),search=ref(''),loading=ref(false),error=ref(''),warning=ref('')
const result=ref<List>({items:[],currentPage:1,pageSize:20,total:0}),page=ref(1),pageSize=ref(20)
const detailOpen=ref(false),detailLoading=ref(false),detailError=ref(''),detail=ref<RecordRow|null>(null)
const kindOptions=[{label:'使用记录',value:'usage'},{label:'错误记录',value:'errors'}]
const rangeOptions=[{label:'最近 1 小时',value:'1'},{label:'最近 24 小时',value:'24'},{label:'最近 7 天',value:'168'},{label:'最近 30 天',value:'720'}]
const columns:BaseTableColumn<RecordRow>[]=[
 {key:'createdAtDisplay',label:'时间',kind:'datetime',size:'xl'},
 {key:'channel',label:'通道',kind:'status',size:'md',fixedWidth:true},
 {key:'route',label:'端点',kind:'mono',size:'2xl'},
 {key:'model',label:'模型',kind:'mono',size:'lg'},
 {key:'account',label:'账号',kind:'identity',size:'xl'},
 {key:'key',label:'密钥',kind:'identity',size:'lg'},
 {key:'reason',label:'原因 / 错误',kind:'text',size:'2xl'},
 {key:'actions',label:'详情',kind:'actions',size:'sm'},
]
const pagination=computed(()=>({currentPage:result.value.currentPage,pageSize:result.value.pageSize,total:result.value.total}))
function channel(r:RecordRow){switch(r.bpsRoute?.channel){case 'basis_points':return 'BPS';case 'native_fallback':return 'BPS回落';case 'rejected':return 'BPS拒绝';case 'bps_error':return 'BPS错误';default:return '未标记'}}
function tone(r:RecordRow):'info'|'warning'|'danger'|'neutral'{switch(r.bpsRoute?.channel){case 'basis_points':return 'info';case 'native_fallback':return 'warning';case 'rejected':case 'bps_error':return 'danger';default:return 'neutral'}}
const reasons:Record<string,string>={inference_dispatched:'',service_tier_unsupported:'服务等级不受 BPS 支持',no_bps_account:'无可用 BPS 账号',bps_failed_before_output:'输出前安全回落',bps_request_failed:'BPS 请求失败',adapter_failed:'适配调用失败',stream_failed:'流式响应失败',cancelled:'请求已取消'}
function reason(r:RecordRow){const code=r.bpsRoute?.reason||'';return r.providerErrorCode||r.failureClass||reasons[code]||(code==='inference_dispatched'?'':code)||((r.message&&r.message!=='succeeded')?r.message:'—')}
function transport(value?:string){return ({websocket:'WS',http_sse:'SSE',http_json:'HTTP'} as Record<string,string>)[value||'']||value||'—'}
async function load(reset=false){
 if(loading.value)return
 if(reset)page.value=1
 loading.value=true;error.value='';warning.value=''
 try{const end=new Date(),start=new Date(end.getTime()-Number(range.value)*3600000);result.value=await request<List>('POST','logs.query',{kind:kind.value,currentPage:page.value,pageSize:pageSize.value,startTime:start.toISOString(),endTime:end.toISOString(),search:search.value});warning.value=result.value.bpsWarning||''}
 catch(e){error.value=e instanceof Error?e.message:'日志读取失败'}finally{loading.value=false}
}
async function showDetail(row:RecordRow){
 detail.value=row;detailOpen.value=true;detailLoading.value=true;detailError.value=''
 try{detail.value=await request<RecordRow>('POST','logs.detail',{id:row.requestId||row.id})}
 catch(e){detailError.value=e instanceof Error?e.message:'详情暂不可用'}finally{detailLoading.value=false}
}
function changePage(value:number){page.value=value;void load()}
function changeSize(value:number){pageSize.value=value;void load(true)}
onMounted(()=>load())
</script>
<template>
 <main class="flex min-h-0 flex-col gap-4 p-4 text-cp-text">
  <BaseCard padding="compact">
   <div class="flex flex-wrap items-center gap-3">
    <BaseSegmented v-model="kind" label="日志类型" :options="kindOptions" :disabled="loading" @update:model-value="load(true)" />
    <BaseSelect v-model="range" :options="rangeOptions" :disabled="loading" aria-label="时间范围" class="w-44" @update:model-value="load(true)" />
    <BaseInput v-model="search" type="search" placeholder="搜索账号、模型或请求" aria-label="搜索日志" :disabled="loading" class="min-w-48 flex-1" @keydown.enter="load(true)" />
    <BaseTag type="neutral">最多 30 天</BaseTag><BaseButton :loading="loading" @click="load(true)">查询</BaseButton>
   </div>
  </BaseCard>
  <div v-if="error" role="alert" class="rounded-cp bg-cp-error-container p-3 text-cp-error-on-container">{{error}}</div>
  <div v-if="warning" role="status" class="rounded-cp bg-cp-warning-container p-3 text-cp-warning-on-container">{{warning}}</div>
  <BaseCard padding="compact">
   <BaseTable :columns="columns" :rows="result.items" :loading="loading" row-key="id" empty-text="该时间范围没有记录" show-header-when-empty class="h-[min(65vh,640px)]" aria-label="BPS 请求日志">
    <template #createdAtDisplay="{row}"><span class="whitespace-nowrap">{{row.createdAtDisplay||row.createdAt||'—'}}</span></template>
    <template #channel="{row}"><BaseTag :type="tone(row)" round :title="row.bpsRoute?'':'没有路由标记不代表请求未经过 BPS'">{{channel(row)}}</BaseTag></template>
    <template #model="{row}">{{row.requestedModel||row.model||'—'}}</template>
    <template #account="{row}">{{row.accountEmail||row.accountName||'—'}}</template>
    <template #key="{row}">{{row.clientApiKeyName||'—'}}</template>
    <template #reason="{row}"><span :title="row.message||reason(row)">{{reason(row)}}</span></template>
    <template #actions="{row}"><BaseButton size="sm" variant="ghost" @click="showDetail(row)">查看</BaseButton></template>
   </BaseTable>
   <BaseTablePagination :pagination="pagination" :loading="loading" @page-change="changePage" @page-size-change="changeSize" />
  </BaseCard>
  <BaseModal v-model="detailOpen" title="请求详情" size="lg">
   <div v-if="detail" class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-3 text-cp-sm">
    <span class="text-cp-text-secondary">请求 ID</span><code class="break-all">{{detail.requestId||detail.id}}</code>
    <span class="text-cp-text-secondary">通道</span><span><BaseTag :type="tone(detail)" round>{{channel(detail)}}</BaseTag></span>
    <span class="text-cp-text-secondary">端点</span><code class="break-all">{{detail.route}}</code>
    <span class="text-cp-text-secondary">模型</span><span>{{detail.requestedModel||detail.model||'—'}}</span>
    <span class="text-cp-text-secondary">账号</span><span class="break-all">{{detail.accountEmail||detail.accountName||'—'}}</span>
    <span class="text-cp-text-secondary">上游 / 客户端</span><span>{{transport(detail.upstreamTransport)}} / {{transport(detail.originalClientTransport||detail.clientTransport)}}</span>
    <span class="text-cp-text-secondary">状态</span><span>{{detail.logicalOutcome||detail.failureClass||'—'}}</span>
    <span class="text-cp-text-secondary">尝试次数</span><span>{{detail.attemptCount??'—'}}</span>
    <span class="text-cp-text-secondary">原因</span><span class="break-all">{{reason(detail)}}</span>
    <template v-if="detail.bpsRoute?.relatedRequestId"><span class="text-cp-text-secondary">关联请求</span><code class="break-all">{{detail.bpsRoute.relatedRequestId}}</code></template>
    <template v-if="detail.message&&detail.message!=='succeeded'"><span class="text-cp-text-secondary">错误详情</span><span class="break-all">{{detail.message}}</span></template>
   </div>
   <p v-if="detailLoading" class="mt-4 text-cp-text-secondary">正在读取详情…</p>
   <p v-if="detailError" role="alert" class="mt-4 text-cp-error-text">{{detailError}}</p>
  </BaseModal>
 </main>
</template>
