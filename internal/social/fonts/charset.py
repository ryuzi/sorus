# JIS X 0208（EUC-JP の2バイト領域）と CP932 拡張、ASCII・ラテン補助・一般句読点・全角形などを列挙する。
chars = set()
for hi in range(0xA1, 0xFF):
    for lo in range(0xA1, 0xFF):
        try:
            chars.add(bytes([hi, lo]).decode('euc_jp'))
        except UnicodeDecodeError:
            pass
for hi in list(range(0x81, 0xA0)) + list(range(0xE0, 0xFD)):
    for lo in list(range(0x40, 0x7F)) + list(range(0x80, 0xFD)):
        try:
            chars.add(bytes([hi, lo]).decode('cp932'))
        except UnicodeDecodeError:
            pass
ranges = [(0x20, 0x7E), (0xA0, 0xFF), (0x2010, 0x205E), (0x2100, 0x215F), (0x2190, 0x21FF),
          (0x2460, 0x24FF), (0x25A0, 0x25FF), (0x2600, 0x266F), (0x3000, 0x30FF), (0xFF01, 0xFF9F)]
for a, b in ranges:
    for cp in range(a, b + 1):
        chars.add(chr(cp))
# 常用漢字のうち JIS X 0208 外の字（𠮟 など）と、よく使う異体字。
chars.update('𠮟塡剝頰叱')
chars = sorted(c for c in chars if len(c) == 1)
open('charset.txt', 'w', encoding='utf-8').write(''.join(chars))
print(len(chars))
