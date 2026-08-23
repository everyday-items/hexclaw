package k12

import "errors"

// ErrModelCapabilityUnverified 表示冻结模型的配置指纹或能力探测回执已缺失、过期或漂移。
// 它在真实 Provider 请求前产生，因此调用方必须终态停止，不能作为网络结果未知重发。
var ErrModelCapabilityUnverified = errors.New("K12_MODEL_CAPABILITY_UNVERIFIED")
