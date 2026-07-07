<script setup lang="ts">
// 3x-ui 面板管理（fork 多面板扩展）。
//
// 视觉与交互完全对齐 Bridges.vue（Bento 卡片网格 + Sheet 抽屉表单 +
// Dialog 删除确认），运维在两页之间切换无认知断点。
//
// 数据流：
//
//   - onMounted refresh()，submit / confirmDelete 后再次 refresh()。
//   - 后端在每个面板上附带 bridge_count（引用计数）：>0 时删除按钮禁用，
//     悬浮提示"先解除引用"，与后端 409 校验双保险。
//
// i18n：所有 panels.* / common.* 文案走 t()。
import { ref, onMounted, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Plus, Pencil, Trash2, Loader2, AlertCircle, Server } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetFooter,
} from '@/components/ui/sheet'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog'
import { Checkbox } from '@/components/ui/checkbox'
import { Alert, AlertDescription } from '@/components/ui/alert'
import LiveDot from '@/components/LiveDot.vue'
import { useToast } from '@/composables/useToast'
import { api, type XuiPanel } from '@/api/client'

const { t } = useI18n()
const { toast } = useToast()

const panels = ref<XuiPanel[]>([])
const loading = ref(true)

// 抽屉表单状态
const drawerOpen = ref(false)
const editingName = ref<string | null>(null)
const formError = ref('')
const submitting = ref(false)
const form = ref({
  name: '',
  api_host: '',
  base_path: '',
  api_token: '',
  timeout_sec: 15,
  skip_tls_verify: false,
})

// 删除确认 Dialog 状态
const deleteOpen = ref(false)
const deletingPanel = ref<XuiPanel | null>(null)

const drawerTitle = computed(() =>
  editingName.value ? t('panels.editTitle', { name: editingName.value }) : t('panels.addTitle'),
)

async function refresh(): Promise<void> {
  loading.value = true
  try {
    panels.value = await api.listPanels()
  } catch (e) {
    console.warn(e)
    toast({ title: t('errors.loadFailed'), variant: 'destructive' })
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  editingName.value = null
  form.value = {
    name: '',
    api_host: '',
    base_path: '',
    api_token: '',
    timeout_sec: 15,
    skip_tls_verify: false,
  }
  formError.value = ''
  drawerOpen.value = true
}

function openEdit(p: XuiPanel): void {
  editingName.value = p.name
  form.value = {
    name: p.name,
    api_host: p.api_host,
    base_path: p.base_path,
    api_token: p.api_token,
    timeout_sec: p.timeout_sec,
    skip_tls_verify: p.skip_tls_verify,
  }
  formError.value = ''
  drawerOpen.value = true
}

async function submit(): Promise<void> {
  formError.value = ''
  if (!form.value.name) {
    formError.value = t('panels.errNameEmpty')
    return
  }
  if (!form.value.api_host) {
    formError.value = t('panels.errHostEmpty')
    return
  }
  submitting.value = true
  try {
    if (editingName.value) {
      await api.updatePanel(editingName.value, form.value)
      toast({ title: t('panels.okUpdated'), variant: 'success' })
    } else {
      await api.createPanel(form.value)
      toast({ title: t('panels.okCreated'), variant: 'success' })
    }
    drawerOpen.value = false
    await refresh()
  } catch (e) {
    void e
    formError.value = t('errors.requestFailed')
  } finally {
    submitting.value = false
  }
}

function askDelete(p: XuiPanel): void {
  if ((p.bridge_count ?? 0) > 0) {
    toast({ title: t('panels.deleteBlocked'), variant: 'warning' })
    return
  }
  deletingPanel.value = p
  deleteOpen.value = true
}

async function confirmDelete(): Promise<void> {
  if (!deletingPanel.value) return
  const target = deletingPanel.value
  deleteOpen.value = false
  try {
    await api.deletePanel(target.name)
    toast({
      title: t('panels.okDeleted', { name: target.name }),
      variant: 'success',
    })
    await refresh()
  } catch (e) {
    void e
    toast({
      title: t('errors.deleteFailed'),
      variant: 'destructive',
    })
  } finally {
    deletingPanel.value = null
  }
}

// 面板"凭据完整"灯：host + token 都非空 → on；否则 warn。
// 与后端 config.Xui.CredsComplete 同语义（前端只做展示，不做真相判定）。
function panelDot(p: XuiPanel): 'on' | 'warn' {
  return p.api_host !== '' && p.api_token !== '' ? 'on' : 'warn'
}

onMounted(refresh)
</script>

<template>
  <div class="space-y-5">
    <!-- 页面头 -->
    <header class="flex items-center justify-between">
      <div>
        <h2 class="text-2xl font-semibold tracking-tight text-foreground">
          {{ t('panels.title') }}
        </h2>
        <p class="mt-1 text-sm text-muted-foreground">
          {{ t('panels.subtitle') }}
        </p>
      </div>
      <Button @click="openCreate">
        <Plus aria-hidden="true" />
        {{ t('panels.addBtn') }}
      </Button>
    </header>

    <!-- 加载占位 -->
    <section v-if="loading && panels.length === 0" class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <Skeleton v-for="n in 3" :key="n" class="h-32 w-full" />
    </section>

    <!-- 空态 -->
    <section
      v-else-if="!loading && panels.length === 0"
      class="bento-tile"
    >
      <div class="rounded-xl border border-dashed bg-muted/30 px-6 py-12 text-center">
        <Server class="mx-auto mb-3 h-10 w-10 text-muted-foreground" aria-hidden="true" />
        <p class="text-sm font-medium text-foreground">{{ t('panels.emptyTitle') }}</p>
        <p class="mt-1 text-xs text-muted-foreground">{{ t('panels.emptyHint') }}</p>
        <Button class="mt-4" @click="openCreate">
          <Plus aria-hidden="true" />
          {{ t('panels.addBtn') }}
        </Button>
      </div>
    </section>

    <!-- 卡片网格 -->
    <section v-else class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <article
        v-for="p in panels"
        :key="p.name"
        class="group relative flex flex-col gap-3 rounded-2xl border bg-card p-5 shadow-bento transition-all duration-200 hover:-translate-y-0.5 hover:border-brand-300 hover:shadow-bento-hover dark:hover:border-brand-700"
      >
        <!-- 顶行：name + 凭据 LiveDot -->
        <div class="flex items-start gap-2">
          <div class="flex-1 min-w-0">
            <p class="truncate text-sm font-semibold text-foreground">{{ p.name }}</p>
            <p class="mt-1.5 truncate font-mono text-xs text-muted-foreground">
              {{ p.api_host || t('common.dash') }}<span v-if="p.base_path">{{ p.base_path }}</span>
            </p>
          </div>
          <LiveDot :status="panelDot(p)" size="md" />
        </div>

        <!-- 中行：引用计数 + 超时 -->
        <div class="grid grid-cols-2 gap-3 text-xs">
          <div>
            <p class="text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
              {{ t('bridges.title') }}
            </p>
            <p class="mt-0.5 font-mono text-foreground">
              {{ t('panels.bridgeCount', { n: p.bridge_count ?? 0 }) }}
            </p>
          </div>
          <div>
            <p class="text-[10px] font-medium uppercase tracking-wider text-muted-foreground">
              {{ t('panels.fieldTimeout') }}
            </p>
            <p class="mt-0.5 font-mono text-foreground">{{ p.timeout_sec }}s</p>
          </div>
        </div>

        <!-- 底行：凭据状态 + 浮按钮 -->
        <div class="flex items-center justify-between border-t pt-3">
          <span class="text-xs font-medium" :class="panelDot(p) === 'on'
            ? 'text-brand-700 dark:text-brand-400'
            : 'text-muted-foreground'">
            {{ panelDot(p) === 'on' ? t('common.configured') : t('common.incomplete') }}
          </span>
          <div class="reveal-on-hover flex gap-1">
            <Button
              variant="ghost"
              size="icon"
              class="h-8 w-8"
              :aria-label="t('common.edit')"
              @click="openEdit(p)"
            >
              <Pencil class="size-4" aria-hidden="true" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              class="h-8 w-8 hover:bg-destructive/10 hover:text-destructive"
              :disabled="(p.bridge_count ?? 0) > 0"
              :aria-label="t('common.delete')"
              @click="askDelete(p)"
            >
              <Trash2 class="size-4" aria-hidden="true" />
            </Button>
          </div>
        </div>
      </article>
    </section>

    <!-- 抽屉表单 -->
    <Sheet v-model:open="drawerOpen">
      <SheetContent side="right" class="flex flex-col">
        <SheetHeader>
          <SheetTitle>{{ drawerTitle }}</SheetTitle>
          <SheetDescription>{{ t('panels.drawerSubtitle') }}</SheetDescription>
        </SheetHeader>

        <form id="panel-form" class="flex-1 space-y-5 overflow-y-auto py-6" @submit.prevent="submit">
          <div>
            <Label for="panel-name">{{ t('panels.fieldName') }}</Label>
            <Input
              id="panel-name"
              v-model="form.name"
              :disabled="!!editingName"
              :placeholder="t('panels.namePlaceholder')"
              class="mt-1.5"
            />
            <p v-if="editingName" class="mt-1.5 text-xs text-muted-foreground">
              {{ t('panels.nameLockedHint') }}
            </p>
          </div>

          <div>
            <Label for="panel-api-host">{{ t('panels.fieldHost') }}</Label>
            <Input
              id="panel-api-host"
              v-model="form.api_host"
              :placeholder="t('panels.hostPlaceholder')"
              class="mt-1.5 font-mono"
            />
          </div>

          <div>
            <Label for="panel-base-path">{{ t('panels.fieldBasePath') }}</Label>
            <Input
              id="panel-base-path"
              v-model="form.base_path"
              :placeholder="t('panels.basePathPlaceholder')"
              class="mt-1.5 font-mono"
            />
          </div>

          <div>
            <Label for="panel-api-token">{{ t('panels.fieldApiToken') }}</Label>
            <Input
              id="panel-api-token"
              v-model="form.api_token"
              type="password"
              autocomplete="off"
              :placeholder="t('panels.apiTokenPlaceholder')"
              class="mt-1.5 font-mono"
            />
            <p class="mt-1.5 text-xs text-muted-foreground">{{ t('panels.apiTokenHelp') }}</p>
          </div>

          <div>
            <Label for="panel-timeout">{{ t('panels.fieldTimeout') }}</Label>
            <Input
              id="panel-timeout"
              v-model.number="form.timeout_sec"
              type="number"
              min="1"
              class="mt-1.5 font-mono"
            />
          </div>

          <div class="flex items-center gap-3">
            <Checkbox id="panel-skip-tls" v-model="form.skip_tls_verify" />
            <Label for="panel-skip-tls" class="cursor-pointer">{{ t('panels.skipTlsLabel') }}</Label>
          </div>

          <Alert v-if="formError" variant="destructive" role="alert" aria-live="assertive">
            <AlertCircle />
            <AlertDescription>{{ formError }}</AlertDescription>
          </Alert>
        </form>

        <SheetFooter class="border-t pt-4">
          <Button type="button" variant="outline" @click="drawerOpen = false">
            {{ t('common.cancel') }}
          </Button>
          <Button type="submit" form="panel-form" :disabled="submitting" :aria-busy="submitting">
            <Loader2 v-if="submitting" class="animate-spin" aria-hidden="true" />
            {{ submitting ? t('common.submitting') : t('common.save') }}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>

    <!-- 删除确认 Dialog -->
    <Dialog v-model:open="deleteOpen">
      <DialogContent class="max-w-md">
        <DialogHeader>
          <DialogTitle>{{ t('common.delete') }}</DialogTitle>
          <DialogDescription>
            {{ deletingPanel ? t('panels.deleteConfirm', { name: deletingPanel.name }) : '' }}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button type="button" variant="outline" @click="deleteOpen = false">
            {{ t('common.cancel') }}
          </Button>
          <Button type="button" variant="destructive" @click="confirmDelete">
            {{ t('common.delete') }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>
