# 生成一个更接近真实生产图纸的测试 DXF（用于验证 cad-service 的增强返回字段）
#
# 包含：
#   - 多图层（图框 / E-FA-设备 / E-文字 / E-标注 / E-表格）
#   - XF-01：标题栏为「图框块 + 属性(ATTRIB)」，设备块带 编号/回路 属性，
#            图例表、主要设备材料表、MTEXT 设计说明（含格式码）、尺寸标注（含文字替代）
#   - XF-02：标题栏为「普通单行文字」（标签 + 值分开写 / "比例 1:100" 写在一起两种）
#   - 图框外文字
#
# 用法：python3 make_sample_v2.py  → fire_alarm_production_sample.dxf
import ezdxf

S = 100  # 1:100 出图，图框放大 100 倍放在模型空间
doc = ezdxf.new("R2010", setup=True)
doc.styles.new("CN", dxfattribs={"font": "simsun.ttf"})
for name, color in [("图框", 7), ("E-FA-设备", 1), ("E-文字", 3), ("E-标注", 2), ("E-表格", 4), ("E-房间", 5)]:
    doc.layers.add(name, color=color)
msp = doc.modelspace()

# ---------- 图框块（A2 594x420），标题栏标签是块内文字，值是属性 ----------
frame = doc.blocks.new("A2图框")
frame.add_lwpolyline([(0, 0), (594, 0), (594, 420), (0, 420)], close=True, dxfattribs={"layer": "图框"})
frame.add_lwpolyline([(25, 10), (584, 10), (584, 410), (25, 410)], close=True, dxfattribs={"layer": "图框"})
frame.add_lwpolyline([(404, 10), (584, 10), (584, 66), (404, 66)], close=True, dxfattribs={"layer": "图框"})
for y in (24, 38, 52):
    frame.add_line((404, y), (584, y), dxfattribs={"layer": "图框"})
frame.add_line((494, 10), (494, 66), dxfattribs={"layer": "图框"})
# 标签 (x, y, 文字)   值属性 (tag, x, y)
LABELS = [("工程名称", 406, 56), ("图名", 406, 42), ("设计", 406, 28), ("审核", 406, 14),
          ("图号", 496, 56), ("比例", 496, 42), ("校对", 496, 28), ("日期", 496, 14)]
for txt, x, y in LABELS:
    frame.add_text(txt, height=2.5, dxfattribs={"style": "CN", "layer": "图框"}).set_placement((x, y))
    frame.add_attdef(tag=txt, insert=(x + 14, y), height=3,
                     dxfattribs={"style": "CN", "layer": "图框", "prompt": txt})

# ---------- 设备块（带属性） ----------
def device_block(name, draw, tags):
    b = doc.blocks.new(name)
    draw(b)
    for i, t in enumerate(tags):
        b.add_attdef(tag=t, insert=(4, 2 - i * 4), height=2.5, dxfattribs={"style": "CN", "layer": "E-FA-设备"})
    return b

device_block("FA-SD", lambda b: (b.add_circle((0, 0), 3, dxfattribs={"layer": "E-FA-设备"}),
                                 b.add_text("S", height=3, dxfattribs={"layer": "E-FA-设备"}).set_placement((-1, -1.5))),
             ["编号", "回路"])
device_block("FA-MCP", lambda b: b.add_lwpolyline([(-3, -3), (3, -3), (3, 3), (-3, 3)], close=True,
                                                   dxfattribs={"layer": "E-FA-设备"}), ["编号", "回路"])
device_block("FA-ALARM", lambda b: b.add_lwpolyline([(-3, -3), (3, -3), (0, 3)], close=True,
                                                     dxfattribs={"layer": "E-FA-设备"}), ["编号"])


def text(s, x, y, h=3.5, layer="E-文字"):
    return msp.add_text(s, height=h * S, dxfattribs={"style": "CN", "layer": layer}).set_placement((x * S, y * S))


def device(block, x, y, attrs, scale=1.0):
    ref = msp.add_blockref(block, (x * S, y * S), dxfattribs={"xscale": S * scale, "yscale": S * scale,
                                                             "layer": "E-FA-设备"})
    ref.add_auto_attribs(attrs)
    return ref


# ======================= XF-01 平面图 =======================
ref = msp.add_blockref("A2图框", (0, 0), dxfattribs={"xscale": S, "yscale": S, "layer": "图框"})
ref.add_auto_attribs({"工程名称": "某某科技园1#楼", "图名": "地下一层火灾自动报警平面图", "图号": "XF-01",
                      "比例": "1:100", "设计": "张伟", "校对": "王芳", "审核": "李强", "日期": "2026.09"})

# 平面内容：房间 + 设备
for name, x, y in [("消防控制室", 80, 330), ("弱电间", 200, 330), ("地下车库", 300, 260), ("设备用房", 140, 230)]:
    text(name, x, y, 5, "E-房间")
for i in range(12):
    device("FA-SD", 60 + (i % 6) * 50, 300 - (i // 6) * 60, {"编号": f"1-{i + 1:02d}", "回路": "L1" if i < 6 else "L2"})
for i in range(4):
    device("FA-MCP", 70 + i * 90, 210, {"编号": f"SB-{i + 1:02d}", "回路": "L1"})
for i in range(3):
    device("FA-ALARM", 90 + i * 110, 190, {"编号": f"SG-{i + 1:02d}"})

# 尺寸标注（轴网尺寸 + 一个文字替代）
dims = [((40, 360), (112, 360)), ((112, 360), (184, 360)), ((184, 360), (268, 360))]
for (p1, p2) in dims:
    msp.add_linear_dim(base=(p1[0] * S, 375 * S), p1=(p1[0] * S, p1[1] * S), p2=(p2[0] * S, p2[1] * S),
                       dimstyle="EZDXF", override={"dimtxt": 2.5 * S, "dimasz": 2 * S},
                       dxfattribs={"layer": "E-标注"}).render()
msp.add_linear_dim(base=(268 * S, 375 * S), p1=(268 * S, 360 * S), p2=(328 * S, 360 * S), text="约6000(现场核实)",
                   dimstyle="EZDXF", override={"dimtxt": 2.5 * S, "dimasz": 2 * S},
                   dxfattribs={"layer": "E-标注"}).render()

# 设计说明（MTEXT，含字体格式码与 \P 换行）
msp.add_mtext("{\\fSimSun|b1;设计说明：}\\P"
              "1. 本工程火灾自动报警系统采用集中报警系统，消防控制室设在地下一层。\\P"
              "2. 感烟探测器吸顶安装，安装间距不大于15m，距墙不小于0.5m。\\P"
              "3. 手动报警按钮底边距地1.5m，声光警报器底边距地2.2m。\\P"
              "4. 报警总线采用WDZN-RYJS-2x1.5，穿SC20钢管暗敷。",
              dxfattribs={"char_height": 3 * S, "width": 160 * S, "style": "CN", "layer": "E-文字"}
              ).set_location((400 * S, 400 * S))

# 图例表（左下）：符号块 + 名称 + 型号 + 安装方式
text("图例", 30, 160, 5, "E-表格")
legend = [("FA-SD", "感烟探测器", "JTY-GD-G3", "吸顶安装"),
          ("FA-MCP", "手动报警按钮", "J-SAM-GST9122", "底边距地1.5m"),
          ("FA-ALARM", "声光警报器", "HX-100B", "底边距地2.2m")]
for i, (blk, name, model, mount) in enumerate(legend):
    y = 150 - i * 10
    msp.add_blockref(blk, (36 * S, (y + 1) * S), dxfattribs={"xscale": S * 0.8, "yscale": S * 0.8, "layer": "E-表格"})
    text(name, 46, y, 3, "E-表格"); text(model, 86, y, 3, "E-表格"); text(mount, 126, y, 3, "E-表格")

# 主要设备材料表（右中）
text("主要设备材料表", 400, 300, 5, "E-表格")
cols = [400, 418, 460, 512, 532, 552]
rows = [("序号", "名称", "型号规格", "单位", "数量", "备注"),
        ("1", "感烟探测器", "JTY-GD-G3", "只", "12", "吸顶"),
        ("2", "手动报警按钮", "J-SAM-GST9122", "只", "4", "带电话插孔"),
        ("3", "声光警报器", "HX-100B", "只", "3", ""),
        ("4", "报警总线", "WDZN-RYJS-2x1.5", "m", "860", "穿SC20")]
for r, row in enumerate(rows):
    for c, cell in enumerate(row):
        if cell:
            text(cell, cols[c], 290 - r * 8, 3, "E-表格")

# ======================= XF-02 系统图（标题栏是普通文字） =======================
OX = 700
msp.add_blockref("A2图框", (OX * S, 0), dxfattribs={"xscale": S, "yscale": S, "layer": "图框"})
for s, x, y, h in [("某某科技园1#楼", 420, 56, 3), ("火灾自动报警系统图", 420, 42, 4), ("张伟", 420, 28, 3),
                   ("李强", 420, 14, 3), ("XF-02", 510, 56, 4), ("比例 1:150", 510, 42, 3), ("王芳", 510, 28, 3),
                   ("2026.09", 510, 14, 3)]:
    text(s, OX + x, y, h, "图框")
for i, s in enumerate(["火灾报警控制器(联动型)  JB-QB-GST5000", "消防控制室图形显示装置", "消防应急广播主机",
                       "消防专用电话总机", "L1回路：地下一层东区 感烟探测器 6只 手动报警按钮 4只",
                       "L2回路：地下一层西区 感烟探测器 6只"]):
    text(s, OX + 60, 360 - i * 14, 4)
for i in range(10):
    msp.add_circle(((OX + 300 + (i % 5) * 30) * S, (250 - (i // 5) * 30) * S), 5 * S, dxfattribs={"layer": "E-FA-设备"})
msp.add_mtext("系统说明：\\P1. 控制器设置在消防控制室，采用壁挂安装。\\P2. 每回路地址点数不超过200点，预留20%余量。",
              dxfattribs={"char_height": 3.5 * S, "width": 200 * S, "style": "CN", "layer": "E-文字"}
              ).set_location(((OX + 60) * S, 250 * S))

# 图框外
msp.add_text("临时备注：本版为施工图审查版", height=5 * S, dxfattribs={"style": "CN"}).set_placement((0, -60 * S))

doc.saveas("fire_alarm_production_sample.dxf")
print("ok")
