/** 判断未知值是否为非数组对象。全项目唯一 canonical 类型守卫，勿在各调用点重建。 */
export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}
