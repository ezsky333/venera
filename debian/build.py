"""Linux (.deb) 打包脚本。

用法：python3 debian/build.py x64|arm64

由 .github/workflows/main.yml 的 linux / linux-arm64 job 调用。

⚠️ 所有 open() 都显式指定 encoding="utf-8"。
debian.yaml 与 venera.desktop 里有中文（描述、名称），
pubspec.yaml 现在也有中文注释。Linux runner 默认 UTF-8 侥幸能过，
显式指定后在任何 locale 下都稳定。
"""
import subprocess
import sys

UTF8 = dict(encoding='utf-8')


def read_text(path):
    with open(path, 'r', **UTF8) as f:
        return f.read()


def write_text(path, text):
    with open(path, 'w', newline='\n', **UTF8) as f:
        f.write(text)


arch = sys.argv[1] if len(sys.argv) > 1 else 'x64'

debian_content = read_text('debian/debian.yaml')
desktop_content = read_text('debian/gui/venera.desktop')

# pubspec 的 version 形如 1.6.3+163，包版本只用前面部分
pubspec = read_text('pubspec.yaml')
version = pubspec.split('version: ')[1].split('\n')[0].split('+')[0].strip()
print(f"version = {version}, arch = {arch}")

rendered = debian_content.replace('{{Version}}', version)
if arch == 'x64':
    rendered = rendered.replace('{{Arch}}', 'x64')
    rendered = rendered.replace('{{Architecture}}', 'amd64')
elif arch == 'arm64':
    rendered = rendered.replace('{{Arch}}', 'arm64')
    rendered = rendered.replace('{{Architecture}}', 'arm64')
else:
    raise SystemExit(f"unsupported arch: {arch}")

write_text('debian/debian.yaml', rendered)
write_text('debian/gui/venera.desktop',
           desktop_content.replace('{{Version}}', version))

try:
    subprocess.run(["flutter", "build", "linux"], check=True)
    subprocess.run(["$HOME/.pub-cache/bin/flutter_to_debian"],
                   shell=True, check=True)
finally:
    # 一定要还原：否则占位符被替换后的版本会留在工作区，
    # 下次构建会找不到 {{Version}} 而静默产出错误版本号。
    write_text('debian/debian.yaml', debian_content)
    write_text('debian/gui/venera.desktop', desktop_content)
