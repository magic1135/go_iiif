# IIIF Image Server 快速开始指南

## 环境要求
- **Go**: 1.20 或更高版本
- **libvips**: 8.10 或更高版本
- **Redis**: 可选 (用于缓存)
- **MinIO**: 可选 (用于对象存储)

# <font color=#FF0099>libvips 环境部署</font>

## <font color=orange face="仿宋">Windows 系统安装 libvips 详细步骤</font>

### 1. 准备工作
- **操作系统版本**: Windows 10 或更高版本（推荐 Windows 11）
- **管理员权限**: 部分步骤可能需要管理员权限
- **VC++ 2022 运行时**([下载链接](https://aka.ms/vs/17/release/vc_redist.x64.exe))
- **网络连接**: 稳定的互联网连接

### 2. 下载预编译二进制文件
1. 访问 [libvips 官方发布页面](https://github.com/libvips/build-win64-mxe/releases/tag/v8.17.0)
2. 在 **Assets** 部分找到:
    - 开发版：vips-dev-8.17.0-windows-x64.zip（含头文件和库）
    - 运行时版：vips-8.17.0-windows-x64.zip（仅运行环境）
3. 下载 ZIP 压缩包到本地目录（例如 `C:\Downloads`）

### 3. 解压安装包
1. 右键点击下载的 ZIP 文件 → "全部解压缩..."
2. 指定目标路径（示例）：
```
C:\vips-8.17.0
├── bin/      # 可执行文件
├── lib/      # 链接库  
├── include/  # C头文件
└── share/    # 资源文件
```
<font color=red>注意：路径中不要有中文或空格（如不要放在"Program Files"下）</font>

### 4. 配置系统环境变量
1. 右键 **"此电脑"** → **"属性"** → **"高级系统设置"**
2. 点击 **"环境变量"**
3. 在 **系统变量** 中找到 `Path` → **"编辑"** → **"新建"**
4. 输入 `C:\vips-8.17.0\bin`（根据实际路径调整）
5. 依次点击 **"确定"** 保存

### 5. 验证： 
> 新开 CMD 窗口，运行 `vips --version`，显示版本号即成功。

### 6. 安装依赖项（可选）
#### 6.1 HEIC 支持
1. 从Microsoft Store安装"HEIF 图像扩展"
2. 或通过winget安装：
> CMD窗口 运行 `winget install Microsoft.HEIFImageExtension `


#### 6.2 ImageMagick（扩展格式支持）
1. 下载 [ImageMagick Windows 版](https://imagemagick.org/script/download.php#windows)
2. 安装时勾选 
   - **"Install development headers and libraries"**
   - **"Add application directory to your system path"**
3. 确保其路径加入 `Path` 环境变量

## <font color=orange face="仿宋">Linux 系统安装 libvips 详细步骤</font>

### 1. 准备工作
- **系统要求**:
  - Ubuntu/Debian: 20.04 LTS 或更高版本
  - RHEL/CentOS: 8 或更高版本
  - 内存：至少2GB（处理大图建议4GB+）

### 2. 通过包管理器安装（推荐）

#### 2.1 Ubuntu/Debian 系列
```bash
# 安装主程序及工具
sudo apt install -y libvips-dev libvips-tools

# 安装常用插件
sudo apt install -y \
  libjpeg-dev libpng-dev libtiff-dev \
  libheif-dev libexif-dev libpoppler-glib-dev
```

#### 2.2 RHEL/CentOS 系列
```bash
# 启用EPEL仓库
sudo dnf install -y epel-release

# 安装主包
sudo dnf install -y vips-devel vips-tools

# 开发依赖
sudo dnf install -y \
  libjpeg-turbo-devel libpng-devel \
  libtiff-devel libheif-devel
```

### 3. 从源码编译安装（高级用户）

#### 3.1 安装构建依赖
```
# Debian系
sudo apt build-dep -y libvips

# RHEL系
sudo dnf groupinstall -y "Development Tools"
```

#### 3.2 编译安装

```bash
# 需要先下载wget(如果没有)
sudo apt install wget
# 下载最新源码
wget https://github.com/libvips/libvips/archive/refs/tags/v8.17.0.tar.gz
# 解压
tar xf vips-8.17.0.tar.gz

cd vips-8.17.0
# 使用 Meson 构建系统配置 libvips 的编译选项
meson setup build --prefix=/usr/local \
  -Dnsgif=true -Dpdfium=enabled -Dheif=enabled
# 编译
ninja -C build
# 安装
sudo ninja -C build install

sudo ldconfig

```
### 4. 验证安装
```bash
vips --version
# 示例输出: vips-8.17.0-Thu Jan 05 14:30:00 UTC 2025
```


## <font color=orange face="仿宋">macOS 系统安装 libvips 详细步骤</font>

### 1. 准备工作
- **硬件要求**: 至少10GB磁盘空间
- **命令行工具**: 需要安装 Xcode Command Line Tools
- **包管理器**: 推荐使用 [Homebrew](https://brew.sh/)（若未安装）

### 2. 安装依赖工具

#### 2.1 安装 Xcode Command Line Tools
```bash
xcode-select --install
```
<font color=red>注:若已安装会提示 "command line tools are already installed"</font>

#### 2.2 安装 Homebrew（如未安装）
```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

#### 2.3安装完成后，将 Homebrew 加入 PATH：
```bash
echo 'eval "$(/opt/homebrew/bin/brew shellenv)"' >> ~/.zshrc
source ~/.zshrc
```
### 3. 通过 Homebrew 安装 libvips

#### 3.1 基础安装( <font color=red>推荐使用国内镜像源下载，避免链接超时问题</font> )
```bash
# 直接安装
brew install vips

# 或使用中科大镜像(国内镜像)安装
HOMEBREW_BOTTLE_DOMAIN=https://mirrors.ustc.edu.cn/homebrew-bottles brew install vips
```
#### 3.2 安装完整功能（推荐）
包含所有可选模块（如 PDF、SVG、HEIC 支持）：
```bash
brew install vips --with-pdfium --with-heif
```
<font color=red>注：新版 Homebrew 已弃用 --with-* 参数，直接安装即可自动包含所有依赖：</font>

```bash
brew install vips
```

### 4. 验证安装

```bash
# 检查版本[README.md](README.md)
vips --version
# 测试图像转换
vips copy input.jpg output.png
# 查看支持格式
vips -l | grep -i format
```
示例输出: `vips-8.17.0-Thu Jan 05 15:30:00 UTC 2025`

# 获取代码
```bash
git clone https://gitlab.com/wang-jiamian/go_iiif.git
cd go_iiif
```
# 配置服务
```yaml
host: "0.0.0.0"
port: 8080
imageDir: "./images"
cacheDir: "./cache"
```

# 项目启动
```bash
 go run main.go
 # 或编译后运行
go build -o iiif-server && ./iiif-server
 ```
[访问http://localhost:8080/](http://localhost:8080/) 
## ***IIIF参数说明***

### 1. 基础URL结构
- {scheme}://{server}{/prefix}/{identifier}/{region}/{size}/{rotation}/{quality}.{format}

### 2. 标识符 (identifier)
- **要求**：唯一标识图片的字符串
- **示例**：
  - `1.jpg`
  - `folder1/image2.png`
### 3. 区域 (region)
| 参数格式          | 示例           | 说明                                                                 |
|-------------------|----------------|----------------------------------------------------------------------|
| `full`            | `full`         | 完整图像                                                           |
| `x,y,w,h`         | `100,200,300,400` | 像素坐标矩形区域 (x,y=左上角坐标，w,h=宽高)                       |
| `pct:x,y,w,h`     | `pct:10,20,30,40` | 百分比区域 (相对于原图尺寸)                                         |
| `square`          | `square`        | 从图像中心截取的最大正方形区域 

### 4. 尺寸 (size)
| 参数格式          | 示例           | 说明                                                                 |
|-------------------|----------------|----------------------------------------------------------------------|
| `max`             | `max`          | 保持原始尺寸                                                        |
| `w,h`             | `300,400`      | 精确尺寸（可能变形）                                                |
| `w,`              | `300,`         | 固定宽度，高度按比例计算                                            |
| `,h`              | `,400`         | 固定高度，宽度按比例计算                                            |
| `pct:n`           | `pct:50`       | 按百分比缩放                                                        |
| `!w,h`            | `!300,400`     | 限制在指定尺寸内的最佳比例（不变形）

### 4. 旋转 (rotation)
| 参数格式  | 示例    | 说明                                                                 |
|-------|-------|----------------------------------------------------------------------|
| `0`   | `0`   | 不旋转                                                              |
| `90`  | `90`  | 图像向右旋转四分之一圈                                                    |
| `180` | `180` | 图像倒置（上下翻转）     
| `270` | `270` | 图像向左旋转四分之一圈     

### 5. 质量 (quality)
| 参数值            | 说明                                                                 |
|-------------------|----------------------------------------------------------------------|
| `default`         | 原始色彩（无修改）                                                  |
| `color`           | 彩色（与default相同）                                               |
| `gray`            | 灰度图像                                                            |
| `bitonal`         | 二值图像（黑白）   

### 6. 格式 (format)
| 格式              | MIME类型           |
|-------------------|-------------------|
| `jpg`             | image/jpg         |
| `png`             | image/png         |
| `webp`            | image/webp        |
| `gif`             | image/gif         |


<p style="color: red;">提示：所有配置修改后需重启服务生效  </p>
<p style="color: red;">文档版本：v1.0.1  </p>
<p style="color: red;">最后更新：2025-07-09</p>

