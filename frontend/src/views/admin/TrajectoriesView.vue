<template>
  <AppLayout>
    <!-- Normal page flow, not TablePageLayout: its fixed viewport height left the table
         0px tall once the summary + filter cards wrapped on narrower screens. -->
    <div class="space-y-6">
      <!-- Archive totals -->
      <div>
        <div v-if="enabled === false" class="card p-6 text-sm text-gray-600 dark:text-gray-300">
          {{ t('admin.trajectories.disabled') }}
        </div>
        <div v-else-if="summary" class="card p-4 sm:p-6">
          <div class="grid grid-cols-2 gap-4 lg:grid-cols-5">
            <div v-for="stat in stats" :key="stat.label">
              <div class="text-xs font-bold uppercase tracking-wider text-gray-400">{{ stat.label }}</div>
              <div class="mt-1 text-xl font-semibold" :class="stat.warn ? 'text-amber-600' : 'text-gray-900 dark:text-white'">
                {{ stat.value }}
              </div>
            </div>
          </div>
          <div v-if="recentDays.length" class="mt-4">
            <div class="mb-1 text-xs text-gray-400">{{ t('admin.trajectories.perDay') }}</div>
            <div class="flex h-16 items-end gap-1">
              <div
                v-for="d in recentDays"
                :key="d.day"
                class="flex-1 rounded-t bg-primary-500/70 hover:bg-primary-600"
                :style="{ height: `${Math.max(4, (d.records / maxDayRecords) * 100)}%` }"
                :title="`${d.day}: ${d.records.toLocaleString()} ${t('admin.trajectories.records')}, ${formatBytes(d.bytes)}`"
              />
            </div>
            <div class="mt-1 flex justify-between text-[11px] text-gray-400">
              <span>{{ recentDays[0].day }}</span>
              <span>{{ recentDays[recentDays.length - 1].day }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Filters -->
      <div>
        <div v-if="enabled" class="card p-4 sm:p-6">
          <div class="flex flex-wrap items-end gap-4">
            <div class="w-full sm:w-auto sm:min-w-[170px]">
              <label class="input-label">{{ t('admin.dashboard.timeRange') }}</label>
              <Select :model-value="timeRange" :options="timeRangeOptions" @update:model-value="handleTimeRangeChange" />
            </div>
            <div class="w-full sm:w-auto sm:min-w-[160px]">
              <label class="input-label">{{ t('admin.trajectories.groupBy') }}</label>
              <Select v-model="groupBy" :options="groupByOptions" @change="search" />
            </div>
            <div v-for="f in textFilters" :key="f.key" class="w-full sm:w-auto" :class="f.wide ? 'sm:min-w-[240px]' : 'sm:min-w-[120px]'">
              <label class="input-label">{{ f.label }}</label>
              <input v-model.trim="filters[f.key]" type="text" class="input" @keyup.enter="search" />
            </div>
            <div class="w-full sm:w-auto sm:min-w-[130px]">
              <label class="input-label">{{ t('admin.trajectories.fields.status') }}</label>
              <Select v-model="filters.status" :options="statusOptions" @change="search" />
            </div>
            <div class="flex flex-wrap items-center gap-3">
              <button type="button" class="btn btn-primary" :disabled="loading" @click="search">{{ t('common.search') }}</button>
              <button type="button" class="btn btn-secondary" :disabled="loading" @click="resetFilters">{{ t('common.reset') }}</button>
              <button type="button" class="btn btn-secondary" :disabled="loading" @click="refreshAll">
                <Icon name="refresh" size="sm" />
              </button>
            </div>
          </div>
          <p class="mt-3 text-xs text-gray-400">{{ t('admin.trajectories.windowHint') }}</p>
        </div>
      </div>

      <!-- Groups -->
      <div class="card overflow-hidden">
        <DataTable v-if="groupBy" :columns="groupColumns" :data="groups" :loading="loading" row-key="key">
          <template #cell-key="{ row }">
            <button
              type="button"
              class="max-w-[320px] truncate text-left font-mono text-sm text-primary-600 hover:underline dark:text-primary-400"
              :title="row.key"
              @click="drillDown(row)"
            >
              {{ row.key || t('admin.trajectories.none') }}
            </button>
            <div v-if="row.models?.length" class="mt-0.5 truncate text-xs text-gray-400">{{ row.models.join(', ') }}</div>
          </template>
          <template #cell-errors="{ row }">
            <span :class="row.errors ? 'text-red-600' : 'text-gray-400'">{{ row.errors }}</span>
          </template>
          <template #cell-avg_latency_ms="{ value }">{{ value }} ms</template>
          <template #cell-bytes="{ row }">{{ formatBytes(row.request_bytes + row.response_bytes) }}</template>
          <template #cell-span="{ row }">
            <span class="whitespace-nowrap text-xs text-gray-500">{{ formatTime(row.first) }} → {{ formatTime(row.last) }}</span>
          </template>
          <template #empty><EmptyState /></template>
        </DataTable>

        <!-- Records -->
        <DataTable v-else :columns="recordColumns" :data="entries" :loading="loading" :row-key="(r: TrajectoryEntry) => `${r.key}#${r.line}`">
          <template #cell-ts="{ value }">
            <span class="whitespace-nowrap text-gray-600 dark:text-gray-300">{{ formatTime(value) }}</span>
          </template>
          <template #cell-request="{ row }">
            <div class="min-w-0 max-w-xs">
              <div class="truncate font-mono text-sm text-gray-800 dark:text-gray-200">{{ row.model || '—' }}</div>
              <div class="mt-0.5 truncate font-mono text-xs text-gray-400" :title="row.path">
                {{ row.path }}<span v-if="row.stream"> · stream</span>
              </div>
            </div>
          </template>
          <template #cell-who="{ row }">
            <div class="whitespace-nowrap text-xs text-gray-500">
              <div>{{ t('admin.trajectories.fields.user') }} {{ row.user_id ?? '—' }} · {{ t('admin.trajectories.fields.apiKey') }} {{ row.api_key_id ?? '—' }}</div>
              <div>{{ t('admin.trajectories.fields.account') }} {{ row.account_id ?? '—' }}</div>
            </div>
          </template>
          <template #cell-session_id="{ value }">
            <button
              v-if="value"
              type="button"
              class="max-w-[160px] truncate font-mono text-xs text-primary-600 hover:underline dark:text-primary-400"
              :title="value"
              @click="openSession(value)"
            >
              {{ value }}
            </button>
            <span v-else class="text-gray-400">—</span>
          </template>
          <template #cell-status="{ value }">
            <span :class="statusBadgeClass(value)">{{ value }}</span>
          </template>
          <template #cell-latency_ms="{ value }">
            <span class="whitespace-nowrap text-gray-500">{{ value }} ms</span>
          </template>
          <template #cell-size="{ row }">
            <span class="whitespace-nowrap text-xs text-gray-500">{{ formatBytes(row.request_bytes) }} / {{ formatBytes(row.response_bytes) }}</span>
          </template>
          <template #cell-actions="{ row }">
            <button
              type="button"
              class="inline-flex items-center gap-1 font-medium text-primary-600 hover:text-primary-700 dark:text-primary-400"
              @click="openDetail(row)"
            >
              <Icon name="eye" size="sm" />
              {{ t('admin.trajectories.view') }}
            </button>
          </template>
          <template #empty><EmptyState /></template>
        </DataTable>
      </div>

      <div>
        <Pagination
          v-if="total > 0"
          :total="total"
          :page="page"
          :page-size="pageSize"
          @update:page="onPageChange"
          @update:pageSize="onPageSizeChange"
        />
      </div>
    </div>

    <!-- Record detail -->
    <BaseDialog :show="detailVisible" :title="t('admin.trajectories.detailTitle')" width="full" :close-on-click-outside="true" @close="detailVisible = false">
      <div v-if="detailLoading" class="flex justify-center py-16">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>
      <div v-else-if="detail" class="space-y-4 py-2">
        <div class="flex flex-wrap items-center gap-x-5 gap-y-1.5 text-xs text-gray-500 dark:text-gray-400">
          <span :class="statusBadgeClass(detail.status)">{{ detail.status }}</span>
          <span class="font-mono">{{ detail.method }} {{ detail.path }}</span>
          <span>{{ formatTime(detail.ts) }}</span>
          <span>{{ detail.latency_ms }} ms</span>
          <span v-if="detail.model" class="font-mono">{{ detail.model }}<template v-if="detail.upstream_model && detail.upstream_model !== detail.model"> → {{ detail.upstream_model }}</template></span>
          <span v-if="detail.request_id">{{ t('admin.trajectories.fields.requestId') }} <span class="font-mono">{{ detail.request_id }}</span></span>
          <span v-if="detail.session_id">{{ t('admin.trajectories.fields.session') }} <span class="font-mono">{{ detail.session_id }}</span></span>
        </div>

        <div class="flex gap-2 border-b border-gray-200 dark:border-dark-700">
          <button
            v-for="tab in detailTabs"
            :key="tab"
            type="button"
            class="-mb-px border-b-2 px-3 py-2 text-sm font-medium"
            :class="detailTab === tab ? 'border-primary-600 text-primary-600' : 'border-transparent text-gray-500 hover:text-gray-700'"
            @click="detailTab = tab"
          >
            {{ t(`admin.trajectories.tabs.${tab}`) }}
          </button>
          <button type="button" class="ml-auto inline-flex items-center gap-1 px-3 py-2 text-sm text-gray-500 hover:text-gray-700" @click="copyDetail">
            <Icon name="copy" size="sm" /> {{ t('admin.trajectories.copy') }}
          </button>
        </div>
        <pre class="max-h-[60vh] overflow-auto whitespace-pre-wrap break-words rounded-xl bg-gray-50 p-4 font-mono text-xs leading-relaxed text-gray-700 dark:bg-dark-900 dark:text-gray-300">{{ detailText }}</pre>
      </div>
    </BaseDialog>

    <!-- Custom time range -->
    <BaseDialog :show="showCustomDialog" :title="t('admin.ops.timeRange.custom')" width="narrow" @close="showCustomDialog = false">
      <div class="space-y-4 py-2">
        <div>
          <label class="input-label">{{ t('admin.ops.customTimeRange.startTime') }}</label>
          <input v-model="customStartInput" type="datetime-local" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.ops.customTimeRange.endTime') }}</label>
          <input v-model="customEndInput" type="datetime-local" class="input" />
        </div>
      </div>
      <template #footer>
        <button type="button" class="btn btn-secondary" @click="showCustomDialog = false">{{ t('common.cancel') }}</button>
        <button type="button" class="btn btn-primary" :disabled="!customStartInput || !customEndInput" @click="confirmCustomRange">
          {{ t('common.confirm') }}
        </button>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import trajectoriesAPI, {
  type TrajectoryEntry,
  type TrajectoryGroup,
  type TrajectoryGroupBy,
  type TrajectoryRecord,
  type TrajectorySummary
} from '@/api/admin/trajectories'
import AppLayout from '@/components/layout/AppLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import type { Column } from '@/components/common/types'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { useAppStore } from '@/stores'

const { t } = useI18n()
const appStore = useAppStore()

const EmptyState = defineComponent(() => () =>
  h('p', { class: 'py-8 text-center text-sm text-gray-500 dark:text-gray-400' }, t('admin.trajectories.empty'))
)

// ---- Summary ----
const enabled = ref<boolean | null>(null)
const summary = ref<TrajectorySummary | null>(null)

const stats = computed(() => {
  const s = summary.value
  if (!s) return []
  return [
    { label: t('admin.trajectories.totalRecords'), value: s.records.toLocaleString() },
    { label: t('admin.trajectories.totalSize'), value: formatBytes(s.bytes) },
    { label: t('admin.trajectories.today'), value: (s.days?.[0]?.records ?? 0).toLocaleString() },
    { label: t('admin.trajectories.pendingUpload'), value: s.spool_files.toLocaleString(), warn: s.spool_files > 5 },
    { label: t('admin.trajectories.dropped'), value: s.dropped.toLocaleString(), warn: s.dropped > 0 }
  ]
})
// Oldest → newest, last 30 days that have data.
const recentDays = computed(() => (summary.value?.days ?? []).slice(0, 30).reverse())
const maxDayRecords = computed(() => Math.max(1, ...recentDays.value.map((d) => d.records)))

async function fetchSummary() {
  try {
    const res = await trajectoriesAPI.summary()
    enabled.value = res.enabled
    summary.value = res.summary ?? null
  } catch (err: any) {
    appStore.showError(err?.message || t('admin.trajectories.loadFailed'))
  }
}

// ---- Query ----
type FilterKey = 'request_id' | 'session_id' | 'model' | 'user_id' | 'api_key_id' | 'account_id' | 'group_id' | 'path' | 'status'
const filters = reactive<Record<FilterKey, string>>({
  request_id: '', session_id: '', model: '', user_id: '', api_key_id: '', account_id: '', group_id: '', path: '', status: ''
})
const groupBy = ref<TrajectoryGroupBy>('')
const order = ref<'asc' | 'desc'>('desc')
const loading = ref(false)
const entries = ref<TrajectoryEntry[]>([])
const groups = ref<TrajectoryGroup[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(50)

const textFilters = computed<{ key: FilterKey; label: string; wide?: boolean }[]>(() => [
  { key: 'request_id', label: t('admin.trajectories.fields.requestId'), wide: true },
  { key: 'session_id', label: t('admin.trajectories.fields.session'), wide: true },
  { key: 'model', label: t('admin.trajectories.fields.model') },
  { key: 'user_id', label: t('admin.trajectories.fields.user') },
  { key: 'api_key_id', label: t('admin.trajectories.fields.apiKey') },
  { key: 'account_id', label: t('admin.trajectories.fields.account') }
])

const groupByOptions = computed(() => [
  { value: '', label: t('admin.trajectories.noGrouping') },
  { value: 'session', label: t('admin.trajectories.fields.session') },
  { value: 'model', label: t('admin.trajectories.fields.model') },
  { value: 'user', label: t('admin.trajectories.fields.user') },
  { value: 'api_key', label: t('admin.trajectories.fields.apiKey') },
  { value: 'account', label: t('admin.trajectories.fields.account') },
  { value: 'group', label: t('admin.trajectories.fields.group') },
  { value: 'path', label: t('admin.trajectories.fields.path') },
  { value: 'status', label: t('admin.trajectories.fields.status') }
])

const statusOptions = computed(() => [
  { value: '', label: t('admin.trajectories.all') },
  { value: 'ok', label: '< 400' },
  { value: 'error', label: '≥ 400' }
])

// Group field → the filter that selects one group (drill-down).
const drillFilter: Record<Exclude<TrajectoryGroupBy, ''>, FilterKey> = {
  session: 'session_id', model: 'model', user: 'user_id', api_key: 'api_key_id',
  account: 'account_id', group: 'group_id', path: 'path', status: 'status'
}

const recordColumns = computed<Column[]>(() => [
  { key: 'ts', label: t('admin.trajectories.fields.time') },
  { key: 'request', label: t('admin.trajectories.fields.request') },
  { key: 'who', label: t('admin.trajectories.fields.who') },
  { key: 'session_id', label: t('admin.trajectories.fields.session') },
  { key: 'status', label: t('admin.trajectories.fields.status') },
  { key: 'latency_ms', label: t('admin.trajectories.fields.latency') },
  { key: 'size', label: t('admin.trajectories.fields.size') },
  { key: 'actions', label: t('common.actions') }
])

const groupColumns = computed<Column[]>(() => [
  { key: 'key', label: groupByOptions.value.find((o) => o.value === groupBy.value)?.label ?? '' },
  { key: 'count', label: t('admin.trajectories.records') },
  { key: 'errors', label: t('admin.trajectories.errors') },
  { key: 'avg_latency_ms', label: t('admin.trajectories.avgLatency') },
  { key: 'bytes', label: t('admin.trajectories.fields.size') },
  { key: 'span', label: t('admin.trajectories.span') }
])

// ---- Time range (max 24h, enforced by the backend too) ----
const RANGE_MINUTES: Record<string, number> = { '15m': 15, '1h': 60, '6h': 360, '24h': 1440 }
const timeRange = ref('1h')
const customStart = ref('')
const customEnd = ref('')
const showCustomDialog = ref(false)
const customStartInput = ref('')
const customEndInput = ref('')

const timeRangeOptions = computed(() => [
  ...Object.keys(RANGE_MINUTES).map((v) => ({ value: v, label: t(`admin.trajectories.ranges.${v}`) })),
  { value: 'custom', label: timeRange.value === 'custom' ? `${t('admin.ops.timeRange.custom')} (${customStart.value.replace('T', ' ')} ~ ${customEnd.value.slice(11)})` : t('admin.ops.timeRange.custom') }
])

function toLocalInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function handleTimeRangeChange(val: string | number | boolean | null) {
  if (val === 'custom') {
    const now = new Date()
    customStartInput.value = customStart.value || toLocalInput(new Date(now.getTime() - 3600_000))
    customEndInput.value = customEnd.value || toLocalInput(now)
    showCustomDialog.value = true
    return
  }
  timeRange.value = String(val)
  search()
}

function confirmCustomRange() {
  const span = new Date(customEndInput.value).getTime() - new Date(customStartInput.value).getTime()
  if (!(span > 0 && span <= 24 * 3600_000)) {
    appStore.showError(t('admin.trajectories.windowHint'))
    return
  }
  customStart.value = customStartInput.value
  customEnd.value = customEndInput.value
  timeRange.value = 'custom'
  showCustomDialog.value = false
  search()
}

function timeWindow(): { start_time: string; end_time: string } {
  if (timeRange.value === 'custom') {
    return { start_time: new Date(customStart.value).toISOString(), end_time: new Date(customEnd.value).toISOString() }
  }
  const end = new Date()
  return { start_time: new Date(end.getTime() - RANGE_MINUTES[timeRange.value] * 60_000).toISOString(), end_time: end.toISOString() }
}

async function fetchRecords() {
  if (!enabled.value) return
  loading.value = true
  try {
    const params: Record<string, string | number> = { page: page.value, page_size: pageSize.value, order: order.value, ...timeWindow() }
    for (const [k, v] of Object.entries(filters)) if (v) params[k] = v
    if (groupBy.value) params.group_by = groupBy.value
    const res = await trajectoriesAPI.records(params)
    if (groupBy.value) groups.value = (res.items ?? []) as TrajectoryGroup[]
    else entries.value = (res.items ?? []) as TrajectoryEntry[]
    total.value = res.total
  } catch (err: any) {
    appStore.showError(err?.message || t('admin.trajectories.loadFailed'))
  } finally {
    loading.value = false
  }
}

function search() {
  page.value = 1
  fetchRecords()
}

function resetFilters() {
  for (const k of Object.keys(filters) as FilterKey[]) filters[k] = ''
  groupBy.value = ''
  order.value = 'desc'
  timeRange.value = '1h'
  search()
}

async function refreshAll() {
  await fetchSummary()
  await fetchRecords()
}

function drillDown(row: TrajectoryGroup) {
  if (!groupBy.value) return
  const field = groupBy.value
  filters[drillFilter[field]] = row.key
  // A session reads best oldest-first, like a conversation.
  order.value = field === 'session' ? 'asc' : 'desc'
  groupBy.value = ''
  search()
}

function openSession(sessionId: string) {
  filters.session_id = sessionId
  order.value = 'asc'
  groupBy.value = ''
  search()
}

function onPageChange(p: number) {
  page.value = p
  fetchRecords()
}

function onPageSizeChange(ps: number) {
  pageSize.value = ps
  search()
}

// ---- Detail ----
const detailTabs = ['text', 'request', 'response', 'headers'] as const
type DetailTab = (typeof detailTabs)[number]
const detailVisible = ref(false)
const detailLoading = ref(false)
const detail = ref<TrajectoryRecord | null>(null)
const detailTab = ref<DetailTab>('text')

async function openDetail(row: TrajectoryEntry) {
  detailVisible.value = true
  detailLoading.value = true
  detail.value = null
  detailTab.value = 'text'
  try {
    detail.value = await trajectoriesAPI.record(row.key, row.line)
  } catch (err: any) {
    appStore.showError(err?.message || t('admin.trajectories.loadFailed'))
    detailVisible.value = false
  } finally {
    detailLoading.value = false
  }
}

const detailText = computed(() => {
  const d = detail.value
  if (!d) return ''
  switch (detailTab.value) {
    case 'request':
      if (d.request_rebuild_error) {
        return `${t('admin.trajectories.rebuildFailed', { error: d.request_rebuild_error })}\n\n${pretty(d.request_delta)}`
      }
      return pretty(d.request_body)
    case 'response':
      return pretty(d.response_body)
    case 'headers':
      return pretty(d.request_headers ?? {})
    default:
      return conversationText(d)
  }
})

function pretty(v: unknown): string {
  if (v == null) return '—'
  return typeof v === 'string' ? v : JSON.stringify(v, null, 2)
}

function copyDetail() {
  navigator.clipboard?.writeText(detailText.value)
  appStore.showSuccess(t('admin.trajectories.copied'))
}

/**
 * Readable view: the last user turn of the request plus the assistant output
 * reassembled from the response. Handles Anthropic Messages, OpenAI Chat and
 * Responses (JSON or SSE); anything else falls back to the raw tabs.
 */
function conversationText(d: TrajectoryRecord): string {
  // Broken delta chain: only this turn's new messages are known.
  const req = (d.request_delta && d.request_body == null
    ? { [d.request_delta.field]: d.request_delta.append }
    : unwrap(d.request_body)) as any
  const parts: string[] = []
  if (d.request_rebuild_error) parts.push(t('admin.trajectories.rebuildFailed', { error: d.request_rebuild_error }))
  const msgs: any[] = req?.messages ?? (Array.isArray(req?.input) ? req.input : Array.isArray(req?.contents) ? req.contents : [])
  const lastUser = [...msgs].reverse().find((m) => m?.role === 'user')
  if (req?.system) parts.push(`[system]\n${contentText(req.system)}`)
  if (typeof req?.input === 'string') parts.push(`[user]\n${req.input}`)
  else if (lastUser) {
    const turns = d.request_delta ? d.request_delta.keep + msgs.length : msgs.length
    parts.push(`[user · turn ${turns}]\n${contentText(lastUser.content)}`)
  }
  parts.push(`[assistant]\n${responseText(unwrap(d.response_body)) || t('admin.trajectories.noText')}`)
  return parts.join('\n\n')
}

function unwrap(v: unknown): unknown {
  const o = v as any
  return o && typeof o === 'object' && o.truncated ? o.data : v
}

function contentText(c: any): string {
  if (typeof c === 'string') return c
  if (!Array.isArray(c)) return c == null ? '' : JSON.stringify(c)
  return c
    .map((p) => p?.text ?? p?.input_text ?? (p?.type === 'tool_result' ? `[tool_result] ${contentText(p.content)}` : p?.type === 'tool_use' ? `[tool_use ${p.name}] ${JSON.stringify(p.input)}` : `[${p?.type ?? 'part'}]`))
    .join('\n')
}

function responseText(body: unknown): string {
  if (typeof body !== 'string') {
    const b = body as any
    if (!b) return ''
    if (b.content) return contentText(b.content) // Anthropic
    if (b.choices) return b.choices.map((c: any) => c.message?.content ?? '').join('\n') // Chat
    if (b.output) return b.output.flatMap((o: any) => (o.content ?? []).map((p: any) => p.text ?? '')).join('') // Responses
    return ''
  }
  // SSE: concatenate text deltas.
  let out = ''
  for (const line of body.split('\n')) {
    if (!line.startsWith('data:')) continue
    try {
      const e = JSON.parse(line.slice(5))
      if (e.type === 'content_block_delta') out += e.delta?.text ?? e.delta?.partial_json ?? '' // Anthropic
      else if (e.type === 'content_block_start' && e.content_block?.type === 'tool_use') out += `\n[tool_use ${e.content_block.name}] `
      else if (e.type === 'response.output_text.delta') out += e.delta ?? '' // Responses
      else if (e.choices) out += e.choices[0]?.delta?.content ?? '' // Chat
    } catch {
      /* [DONE] and non-JSON frames */
    }
  }
  return out
}

// ---- Helpers ----
function formatTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

function formatBytes(n: number): string {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${units[i]}`
}

function statusBadgeClass(status: number): string {
  const base = 'inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold '
  if (status >= 500) return base + 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (status >= 400) return base + 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'
  return base + 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
}

onMounted(async () => {
  await fetchSummary()
  await fetchRecords()
})
</script>
