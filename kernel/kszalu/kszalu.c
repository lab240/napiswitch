// One-shot dump of the KSZ9477-family static ALU table through the ksz
// driver's 32-bit regmap. Debug only: loads, logs, refuses to stay.
#include <linux/module.h>
#include <linux/spi/spi.h>
#include <linux/regmap.h>

#define REG_SW_ALU_STAT_CTRL__4	0x041C
#define REG_SW_ALU_VAL_A	0x0420
#define ALU_STAT_START		BIT(7)
#define ALU_STAT_READ		1
#define ALU_STAT_INDEX_S	16

static char *dev_name_p = "spi2.0";
module_param_named(dev, dev_name_p, charp, 0);
static uint entries = 16;
module_param(entries, uint, 0);

static int __init kszalu_init(void)
{
	struct device *d;
	struct regmap *map;
	unsigned int v, t[4];
	int i, j, err = 0;

	d = bus_find_device_by_name(&spi_bus_type, NULL, dev_name_p);
	if (!d)
		return -ENODEV;
	map = dev_get_regmap(d, "32");
	if (!map) {
		put_device(d);
		return -ENXIO;
	}
	for (i = 0; i < entries && !err; i++) {
		err = regmap_write(map, REG_SW_ALU_STAT_CTRL__4,
				   (i << ALU_STAT_INDEX_S) | ALU_STAT_READ | ALU_STAT_START);
		if (!err)
			err = regmap_read_poll_timeout(map, REG_SW_ALU_STAT_CTRL__4, v,
						       !(v & ALU_STAT_START), 10, 1000);
		for (j = 0; j < 4 && !err; j++)
			err = regmap_read(map, REG_SW_ALU_VAL_A + 4 * j, &t[j]);
		if (!err && (t[0] & BIT(31)))
			pr_info("kszalu: [%2d] A=%08x B=%08x C=%08x D=%08x mac=%04x%08x ports=%06x%s fid=%u\n",
				i, t[0], t[1], t[2], t[3], t[2] & 0xffff, t[3],
				t[1] & 0xffffff, t[1] & BIT(31) ? " OVERRIDE" : "", t[2] >> 16);
	}
	pr_info("kszalu: %s %u static entries scanned, err %d\n", dev_name_p, i, err);
	put_device(d);
	return err ? err : -EAGAIN; /* never stay loaded */
}

module_init(kszalu_init);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("one-shot KSZ static ALU table dump for debugging");
