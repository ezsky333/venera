"""Windows 构建与打包脚本。

由 .github/workflows/main.yml 的 windows job 调用。

⚠️ 所有 open() 都必须显式指定 encoding="utf-8"。
Windows 上 Python 的默认编码是 cp1252（GitHub runner 也是），
而 pubspec.yaml / build.iss 里有中文（注释、简体中文安装包文案），
用默认编码读会直接抛：

    UnicodeDecodeError: 'charmap' codec can't decode byte 0x90 ...

这个坑是在给 pubspec.yaml 补中文注释后踩到的：本地是 GBK/cp936
环境尚能蒙混，CI 的 cp1252 一律失败。
"""
import os
import subprocess

import httpx


def read_text(path):
    with open(path, 'r', encoding='utf-8') as f:
        return f.read()


def write_text(path, text):
    with open(path, 'w', encoding='utf-8', newline='\n') as f:
        f.write(text)


content = read_text('pubspec.yaml')

subprocess.run(["flutter", "build", "windows"], shell=True, check=True)

if os.path.exists("build/app-windows.zip"):
    os.remove("build/app-windows.zip")

# pubspec 的 version 形如 1.6.3+163，文件名只取前面的语义化版本
version = content.split('version: ')[1].split('\n')[0].split('+')[0].strip()
print(f"version = {version}")

os.makedirs("build/windows", exist_ok=True)
subprocess.run(
    ["tar", "-a", "-c", "-f",
     f"build/windows/Venera-{version}-windows.zip",
     "-C", "build/windows/x64/runner/Release", "*"],
    shell=True, check=True)

# build.iss 里带 {{version}} / {{root_path}} 占位符，替换后编译，
# 编译完再还原，避免工作区里留下被改写的文件（否则下次 git diff 有噪音）
iss_original = read_text('windows/build.iss')
iss_rendered = (iss_original
                .replace("{{version}}", version)
                .replace("{{root_path}}", os.getcwd()))
write_text('windows/build.iss', iss_rendered)

try:
    isl = "windows/ChineseSimplified.isl"
    if not os.path.exists(isl):
        url = ("https://cdn.jsdelivr.net/gh/kira-96/"
               "Inno-Setup-Chinese-Simplified-Translation@latest/"
               "ChineseSimplified.isl")
        response = httpx.get(url, follow_redirects=True, timeout=60)
        response.raise_for_status()
        with open(isl, 'wb') as f:
            f.write(response.content)

    subprocess.run(["iscc", "windows/build.iss"], shell=True, check=True)
finally:
    # 无论如何都还原，保证幂等
    write_text('windows/build.iss', iss_original)
