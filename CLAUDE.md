# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 常用命令

### 开发与运行
- `make run` - 构建前端并运行服务器（生产模式）
- `make dev` - 以开发模式运行（带竞态检测）
- `go run ./main.go` - 直接运行后端服务器

### 前端开发（在 web/ 目录下）
- `npm install` - 安装前端依赖
- `npm run dev` - 启动前端开发服务器
- `npm run build` - 构建生产版本前端
- `npm run lint` - 运行 ESLint 代码检查
- `npm run type-check` - 执行 TypeScript 类型检查

### 测试
- 项目当前没有配置单元测试框架

## 项目架构

### 整体架构
GPT-Load 是一个高性能的 AI 接口透明代理服务，采用 Go + Vue 3 全栈架构：

- **后端**: Go 1.23+，使用 Gin 框架，依赖注入（uber-go/dig）
- **前端**: Vue 3 + TypeScript + Vite + Naive UI
- **数据库**: 支持 SQLite、MySQL、PostgreSQL
- **缓存**: Redis（可选，不配置时使用内存存储）

### 后端核心模块（internal/）

#### 应用层
- `app/` - 应用生命周期管理，主从节点协调
- `container/` - 依赖注入容器配置

#### 代理核心
- `proxy/` - 代理服务器实现，请求转发和流式处理
- `channel/` - 多种 AI 服务适配器（OpenAI、Gemini、Anthropic）
- `keypool/` - 智能密钥池管理，支持轮换和故障恢复

#### 配置管理
- `config/` - 系统配置和热重载管理
- `models/` - 数据模型定义
- `types/` - 核心类型定义

#### 服务层
- `services/` - 业务服务（分组管理、密钥服务、日志服务等）
- `handler/` - HTTP 路由处理器
- `middleware/` - HTTP 中间件

#### 基础设施
- `store/` - 存储抽象层（内存/Redis）
- `db/` - 数据库迁移和连接管理
- `httpclient/` - HTTP 客户端管理和连接池
- `utils/` - 工具函数

### 前端架构（web/src/）

#### 核心结构
- `components/` - Vue 组件库
  - `keys/` - 密钥管理相关组件
  - `logs/` - 日志查看组件
  - `common/` - 通用组件
- `views/` - 页面级组件
- `api/` - API 调用封装
- `router/` - 路由配置
- `types/` - TypeScript 类型定义

### 代理工作流程
1. 请求路由：`/proxy/{group_name}/{api_path}` 
2. 认证验证：检查代理密钥
3. 分组解析：根据 group_name 获取配置
4. 密钥选择：从密钥池选择可用密钥
5. 通道适配：根据分组类型选择对应 channel
6. 请求转发：负载均衡转发到上游服务
7. 响应处理：流式或非流式响应处理
8. 日志记录：异步记录请求日志

### 主从架构
- Master 节点：数据库迁移、后台任务、密钥验证
- Slave 节点：仅处理代理请求
- 配置环境变量 `IS_SLAVE=true` 启用从节点模式

### 配置系统
双层配置架构：
- 静态配置：环境变量，需要重启生效
- 动态配置：数据库存储，支持热重载
- 优先级：分组配置 > 系统设置 > 环境配置