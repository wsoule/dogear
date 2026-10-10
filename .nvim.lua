vim.opt.tabstop = 2
vim.opt.shiftwidth = 2
vim.opt.expandtab = true
vim.g.ale_fixers = {
	typescript = { "oxfmt" },
	javascript = { "oxfmt" },
	typescriptreact = { "oxfmt" },
	javascriptreact = { "oxfmt" },
	json = { "oxfmt" },
	jsonc = { "oxfmt" },
	css = { "oxfmt" },
	markdown = { "oxfmt" },
	html = { "oxfmt" },
	mdx = { "oxfmt" },
	conf = { "oxfmt" },
	lua = { "stylua" },
}

vim.g.ale_fix_on_save = 1
