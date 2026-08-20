<template>
  <el-dialog :model-value="visible" title="修改密码" width="420px" @update:model-value="emit('update:visible', $event)" @closed="reset">
    <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
      <el-form-item label="旧密码" prop="oldPassword">
        <el-input v-model="form.oldPassword" type="password" show-password />
      </el-form-item>
      <el-form-item label="新密码" prop="newPassword">
        <el-input v-model="form.newPassword" type="password" show-password />
      </el-form-item>
      <el-form-item label="确认新密码" prop="confirm">
        <el-input v-model="form.confirm" type="password" show-password />
      </el-form-item>
      <div class="hint">SSO 登录的账号未设置本地密码，无法在此修改；如需本地口令请联系管理员重置。</div>
    </el-form>
    <template #footer>
      <el-button @click="emit('update:visible', false)">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">确定</el-button>
    </template>
  </el-dialog>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { authApi } from '@/api'

defineProps<{ visible: boolean }>()
const emit = defineEmits<{ (e: 'update:visible', v: boolean): void }>()

const formRef = ref<FormInstance>()
const saving = ref(false)
const form = reactive({ oldPassword: '', newPassword: '', confirm: '' })

const rules: FormRules = {
  oldPassword: [{ required: true, message: '请输入旧密码', trigger: 'blur' }],
  newPassword: [{ required: true, message: '请输入新密码', trigger: 'blur' }],
  confirm: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_rule, value: string, callback) => {
        if (value !== form.newPassword) callback(new Error('两次输入的新密码不一致'))
        else callback()
      },
      trigger: 'blur',
    },
  ],
}

function reset() {
  Object.assign(form, { oldPassword: '', newPassword: '', confirm: '' })
  formRef.value?.clearValidate()
}

async function save() {
  await formRef.value?.validate()
  saving.value = true
  try {
    await authApi.changePassword({ old_password: form.oldPassword, new_password: form.newPassword })
    ElMessage.success('密码已修改')
    emit('update:visible', false)
  } catch {
    // 错误已由 http.ts 提示（旧密码错误 / SSO 账号无本地密码等）
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.hint {
  margin-left: 16px;
  color: var(--el-text-color-placeholder);
  font-size: 12px;
  line-height: 1.6;
}
</style>
