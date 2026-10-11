# 生成一个带 2 个 A3 图框 + 中文标题栏的测试 DXF
import ezdxf
doc = ezdxf.new("R2010", setup=True)
doc.styles.new("CN", dxfattribs={"font": "simsun.ttf"})
msp = doc.modelspace()
S = 100  # 模型空间 1:100 出图比例（真实图纸的图框通常放大到这个量级）
# 图框做成块（真实图纸里图框几乎都是块参照，go-cad 也是按 INSERT 识别图框）
blk = doc.blocks.new("A3图框")
blk.add_lwpolyline([(0,0),(420,0),(420,297),(0,297)], close=True)
blk.add_lwpolyline([(10,10),(410,10),(410,287),(10,287)], close=True)
blk.add_lwpolyline([(230,10),(410,10),(410,50),(230,50)], close=True)  # 标题栏
blk.add_line((230,30),(410,30)); blk.add_line((320,10),(320,50))
def frame(ox, no, title, items):
    msp.add_blockref("A3图框", (ox*S, 0), dxfattribs={"xscale": S, "yscale": S})
    for i in range(10): msp.add_circle(((ox+60+i*30)*S, 200*S), 5*S)  # 图形内容
    t = lambda s,x,y,h=3.5: msp.add_text(s, dxfattribs={"height":h*S,"style":"CN"}).set_placement(((ox+x)*S,y*S))
    # 标题栏排布：图名在上、图号在下，均位于图框右下角并右对齐（常见设计院样式）
    t("设计单位：某某建筑设计研究院", 235, 38, 2.5); t("比例 1:100", 235, 18, 2.5)
    t("图名", 322, 40, 2.5); t(title, 410 - 4*len(title) - 4, 38, 4)
    t("图号", 322, 20, 2.5); t(no, 410 - 0.6*4*len(no) - 4, 18, 4)
    y = 270
    for line in items:
        t(line, 20, y, 3.5); y -= 8
    for i in range(6): t("烟感探测器", 40+i*25, 120, 2.5)
    for v in ["3600", "1500", "φ25", "2400,00"]: t(v, 60+len(v)*20, 90, 2.5)
frame(0, "XF-01", "地下一层消防平面图",
      ["设计说明：", "1. 本工程火灾自动报警系统采用集中报警系统。",
       "2. 消防控制室设在首层，24小时有人值班。",
       "3. 感烟探测器安装间距不大于15米，距墙不小于0.5米。",
       "4. 手动报警按钮安装高度距地1.5米。"])
frame(500, "XF-02", "首层弱电系统图",
      ["弱电系统说明：", "1. 综合布线采用六类非屏蔽双绞线。",
       "2. 弱电井内设置接地端子箱，接地电阻不大于1欧姆。",
       "3. 视频监控采用400万像素网络摄像机。"])
doc.saveas("fire_weak_current_sample.dxf")
print("ok")
